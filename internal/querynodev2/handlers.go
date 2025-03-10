// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package querynodev2

import (
	"context"
	"fmt"
	"strconv"

	"github.com/golang/protobuf/proto"
	"github.com/samber/lo"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus/internal/proto/internalpb"
	"github.com/milvus-io/milvus/internal/proto/planpb"
	"github.com/milvus-io/milvus/internal/proto/querypb"
	"github.com/milvus-io/milvus/internal/querynodev2/delegator"
	"github.com/milvus-io/milvus/internal/querynodev2/segments"
	"github.com/milvus-io/milvus/internal/querynodev2/tasks"
	"github.com/milvus-io/milvus/internal/util/streamrpc"
	"github.com/milvus-io/milvus/pkg/common"
	"github.com/milvus-io/milvus/pkg/log"
	"github.com/milvus-io/milvus/pkg/metrics"
	"github.com/milvus-io/milvus/pkg/util/funcutil"
	"github.com/milvus-io/milvus/pkg/util/merr"
	"github.com/milvus-io/milvus/pkg/util/paramtable"
	"github.com/milvus-io/milvus/pkg/util/timerecord"
)

func loadGrowingSegments(ctx context.Context, delegator delegator.ShardDelegator, req *querypb.WatchDmChannelsRequest) error {
	// load growing segments
	growingSegments := make([]*querypb.SegmentLoadInfo, 0, len(req.Infos))
	for _, info := range req.Infos {
		for _, segmentID := range info.GetUnflushedSegmentIds() {
			// unFlushed segment may not have binLogs, skip loading
			segmentInfo := req.GetSegmentInfos()[segmentID]
			if segmentInfo == nil {
				log.Warn("an unflushed segment is not found in segment infos", zap.Int64("segmentID", segmentID))
				continue
			}
			if len(segmentInfo.GetBinlogs()) > 0 {
				growingSegments = append(growingSegments, &querypb.SegmentLoadInfo{
					SegmentID:     segmentInfo.ID,
					PartitionID:   segmentInfo.PartitionID,
					CollectionID:  segmentInfo.CollectionID,
					BinlogPaths:   segmentInfo.Binlogs,
					NumOfRows:     segmentInfo.NumOfRows,
					Statslogs:     segmentInfo.Statslogs,
					Deltalogs:     segmentInfo.Deltalogs,
					InsertChannel: segmentInfo.InsertChannel,
				})
			} else {
				log.Info("skip segment which binlog is empty", zap.Int64("segmentID", segmentInfo.ID))
			}
		}
	}

	return delegator.LoadGrowing(ctx, growingSegments, req.GetVersion())
}

func (node *QueryNode) loadDeltaLogs(ctx context.Context, req *querypb.LoadSegmentsRequest) *commonpb.Status {
	log := log.Ctx(ctx).With(
		zap.Int64("collectionID", req.GetCollectionID()),
	)

	var finalErr error
	for _, info := range req.GetInfos() {
		segment := node.manager.Segment.GetSealed(info.GetSegmentID())
		if segment == nil {
			continue
		}

		local := segment.(*segments.LocalSegment)
		err := node.loader.LoadDeltaLogs(ctx, local, info.GetDeltalogs())
		if err != nil {
			if finalErr == nil {
				finalErr = err
			}
			continue
		}
	}

	if finalErr != nil {
		log.Warn("failed to load delta logs", zap.Error(finalErr))
		return merr.Status(finalErr)
	}

	return merr.Success()
}

func (node *QueryNode) loadIndex(ctx context.Context, req *querypb.LoadSegmentsRequest) *commonpb.Status {
	log := log.Ctx(ctx).With(
		zap.Int64("collectionID", req.GetCollectionID()),
		zap.Int64s("segmentIDs", lo.Map(req.GetInfos(), func(info *querypb.SegmentLoadInfo, _ int) int64 { return info.GetSegmentID() })),
	)

	status := merr.Success()
	log.Info("start to load index")

	for _, info := range req.GetInfos() {
		log := log.With(zap.Int64("segmentID", info.GetSegmentID()))
		segment := node.manager.Segment.GetSealed(info.GetSegmentID())
		if segment == nil {
			log.Warn("segment not found for load index operation")
			continue
		}
		localSegment, ok := segment.(*segments.LocalSegment)
		if !ok {
			log.Warn("segment not local for load index opeartion")
			continue
		}

		err := node.loader.LoadIndex(ctx, localSegment, info, req.Version)
		if err != nil {
			log.Warn("failed to load index", zap.Error(err))
			status = merr.Status(err)
			break
		}
	}

	return status
}

func (node *QueryNode) queryChannel(ctx context.Context, req *querypb.QueryRequest, channel string) (*internalpb.RetrieveResults, error) {
	msgID := req.Req.Base.GetMsgID()
	traceID := trace.SpanFromContext(ctx).SpanContext().TraceID()
	log := log.Ctx(ctx).With(
		zap.Int64("msgID", msgID),
		zap.Int64("collectionID", req.GetReq().GetCollectionID()),
		zap.String("channel", channel),
		zap.String("scope", req.GetScope().String()),
	)

	var err error
	metrics.QueryNodeSQCount.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), metrics.QueryLabel, metrics.TotalLabel, metrics.Leader).Inc()
	defer func() {
		if err != nil {
			metrics.QueryNodeSQCount.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), metrics.QueryLabel, metrics.FailLabel, metrics.Leader).Inc()
		}
	}()

	log.Debug("start do query with channel",
		zap.Bool("fromShardLeader", req.GetFromShardLeader()),
		zap.Int64s("segmentIDs", req.GetSegmentIDs()),
	)
	// add cancel when error occurs
	queryCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// From Proxy
	tr := timerecord.NewTimeRecorder("queryDelegator")
	// get delegator
	sd, ok := node.delegators.Get(channel)
	if !ok {
		err := merr.WrapErrChannelNotFound(channel)
		log.Warn("Query failed, failed to get shard delegator for query", zap.Error(err))
		return nil, err
	}

	// do query
	results, err := sd.Query(queryCtx, req)
	if err != nil {
		log.Warn("failed to query on delegator", zap.Error(err))
		return nil, err
	}

	// reduce result
	tr.CtxElapse(ctx, fmt.Sprintf("start reduce query result, traceID = %s, fromSharedLeader = %t, vChannel = %s, segmentIDs = %v",
		traceID,
		req.GetFromShardLeader(),
		channel,
		req.GetSegmentIDs(),
	))

	collection := node.manager.Collection.Get(req.Req.GetCollectionID())
	if collection == nil {
		err := merr.WrapErrCollectionNotFound(req.Req.GetCollectionID())
		log.Warn("Query failed, failed to get collection", zap.Error(err))
		return nil, err
	}

	reducer := segments.CreateInternalReducer(req, collection.Schema())

	resp, err := reducer.Reduce(ctx, results)
	if err != nil {
		return nil, err
	}

	tr.CtxElapse(ctx, fmt.Sprintf("do query with channel done , vChannel = %s, segmentIDs = %v",
		channel,
		req.GetSegmentIDs(),
	))

	latency := tr.ElapseSpan()
	metrics.QueryNodeSQReqLatency.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), metrics.QueryLabel, metrics.Leader).Observe(float64(latency.Milliseconds()))
	metrics.QueryNodeSQCount.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), metrics.QueryLabel, metrics.SuccessLabel, metrics.Leader).Inc()
	return resp, nil
}

func (node *QueryNode) queryChannelStream(ctx context.Context, req *querypb.QueryRequest, channel string, srv streamrpc.QueryStreamServer) error {
	metrics.QueryNodeSQCount.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), metrics.QueryLabel, metrics.TotalLabel, metrics.Leader).Inc()
	msgID := req.Req.Base.GetMsgID()
	log := log.Ctx(ctx).With(
		zap.Int64("msgID", msgID),
		zap.Int64("collectionID", req.GetReq().GetCollectionID()),
		zap.String("channel", channel),
		zap.String("scope", req.GetScope().String()),
	)

	var err error
	defer func() {
		if err != nil {
			metrics.QueryNodeSQCount.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), metrics.QueryLabel, metrics.FailLabel, metrics.Leader).Inc()
		}
	}()

	log.Debug("start do streaming query with channel",
		zap.Bool("fromShardLeader", req.GetFromShardLeader()),
		zap.Int64s("segmentIDs", req.GetSegmentIDs()),
	)

	// add cancel when error occurs
	queryCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// From Proxy
	tr := timerecord.NewTimeRecorder("queryDelegator")
	// get delegator
	sd, ok := node.delegators.Get(channel)
	if !ok {
		err := merr.WrapErrChannelNotFound(channel)
		log.Warn("Query failed, failed to get query shard delegator", zap.Error(err))
		return err
	}

	// do query
	err = sd.QueryStream(queryCtx, req, srv)
	if err != nil {
		return err
	}

	tr.CtxElapse(ctx, fmt.Sprintf("do query with channel done , vChannel = %s, segmentIDs = %v",
		channel,
		req.GetSegmentIDs(),
	))

	return nil
}

func (node *QueryNode) queryStreamSegments(ctx context.Context, req *querypb.QueryRequest, srv streamrpc.QueryStreamServer) error {
	collection := node.manager.Collection.Get(req.Req.GetCollectionID())
	if collection == nil {
		return merr.WrapErrCollectionNotFound(req.Req.GetCollectionID())
	}

	// Send task to scheduler and wait until it finished.
	task := tasks.NewQueryStreamTask(ctx, collection, node.manager, req, srv)
	if err := node.scheduler.Add(task); err != nil {
		log.Warn("failed to add query task into scheduler", zap.Error(err))
		return err
	}

	err := task.Wait()
	if err != nil {
		log.Warn("failed to execute task by node scheduler", zap.Error(err))
		return err
	}

	return nil
}

func (node *QueryNode) optimizeSearchParams(ctx context.Context, req *querypb.SearchRequest, deleg delegator.ShardDelegator) (*querypb.SearchRequest, error) {
	// no hook applied, just return
	if node.queryHook == nil {
		return req, nil
	}

	log := log.Ctx(ctx).With(zap.Int64("collection", req.GetReq().GetCollectionID()))

	serializedPlan := req.GetReq().GetSerializedExprPlan()
	// plan not found
	if serializedPlan == nil {
		log.Warn("serialized plan not found")
		return req, merr.WrapErrParameterInvalid("serialized search plan", "nil")
	}

	channelNum := req.GetTotalChannelNum()
	// not set, change to conservative channel num 1
	if channelNum <= 0 {
		channelNum = 1
	}

	plan := planpb.PlanNode{}
	err := proto.Unmarshal(serializedPlan, &plan)
	if err != nil {
		log.Warn("failed to unmarshal plan", zap.Error(err))
		return nil, merr.WrapErrParameterInvalid("valid serialized search plan", "no unmarshalable one", err.Error())
	}

	switch plan.GetNode().(type) {
	case *planpb.PlanNode_VectorAnns:
		// ignore growing ones for now since they will always be brute force
		sealed, _ := deleg.GetSegmentInfo(true)
		sealedNum := lo.Reduce(sealed, func(sum int, item delegator.SnapshotItem, _ int) int {
			return sum + len(item.Segments)
		}, 0)
		// use shardNum * segments num in shard to estimate total segment number
		estSegmentNum := sealedNum * int(channelNum)
		withFilter := (plan.GetVectorAnns().GetPredicates() != nil)
		queryInfo := plan.GetVectorAnns().GetQueryInfo()
		params := map[string]any{
			common.TopKKey:        queryInfo.GetTopk(),
			common.SearchParamKey: queryInfo.GetSearchParams(),
			common.SegmentNumKey:  estSegmentNum,
			common.WithFilterKey:  withFilter,
			common.CollectionKey:  req.GetReq().GetCollectionID(),
		}
		err := node.queryHook.Run(params)
		if err != nil {
			log.Warn("failed to execute queryHook", zap.Error(err))
			return nil, merr.WrapErrServiceUnavailable(err.Error(), "queryHook execution failed")
		}
		queryInfo.Topk = params[common.TopKKey].(int64)
		queryInfo.SearchParams = params[common.SearchParamKey].(string)
		serializedExprPlan, err := proto.Marshal(&plan)
		if err != nil {
			log.Warn("failed to marshal optimized plan", zap.Error(err))
			return nil, merr.WrapErrParameterInvalid("marshalable search plan", "plan with marshal error", err.Error())
		}
		req.Req.SerializedExprPlan = serializedExprPlan
		log.Debug("optimized search params done", zap.Any("queryInfo", queryInfo))
	default:
		log.Warn("not supported node type", zap.String("nodeType", fmt.Sprintf("%T", plan.GetNode())))
	}
	return req, nil
}

func (node *QueryNode) searchChannel(ctx context.Context, req *querypb.SearchRequest, channel string) (*internalpb.SearchResults, error) {
	log := log.Ctx(ctx).With(
		zap.Int64("msgID", req.GetReq().GetBase().GetMsgID()),
		zap.Int64("collectionID", req.Req.GetCollectionID()),
		zap.String("channel", channel),
		zap.String("scope", req.GetScope().String()),
	)
	traceID := trace.SpanFromContext(ctx).SpanContext().TraceID()

	if err := node.lifetime.Add(merr.IsHealthy); err != nil {
		return nil, err
	}
	defer node.lifetime.Done()

	var err error
	metrics.QueryNodeSQCount.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), metrics.SearchLabel, metrics.TotalLabel, metrics.Leader).Inc()
	defer func() {
		if err != nil {
			metrics.QueryNodeSQCount.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), metrics.SearchLabel, metrics.FailLabel, metrics.Leader).Inc()
		}
	}()

	log.Debug("start to search channel",
		zap.Bool("fromShardLeader", req.GetFromShardLeader()),
		zap.Int64s("segmentIDs", req.GetSegmentIDs()),
	)
	searchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// From Proxy
	tr := timerecord.NewTimeRecorder("searchDelegator")
	// get delegator
	sd, ok := node.delegators.Get(channel)
	if !ok {
		err := merr.WrapErrChannelNotFound(channel)
		log.Warn("Query failed, failed to get shard delegator for search", zap.Error(err))
		return nil, err
	}
	req, err = node.optimizeSearchParams(ctx, req, sd)
	if err != nil {
		log.Warn("failed to optimize search params", zap.Error(err))
		return nil, err
	}
	// do search
	results, err := sd.Search(searchCtx, req)
	if err != nil {
		log.Warn("failed to search on delegator", zap.Error(err))
		return nil, err
	}

	// reduce result
	tr.CtxElapse(ctx, fmt.Sprintf("start reduce query result, traceID = %s, fromSharedLeader = %t, vChannel = %s, segmentIDs = %v",
		traceID,
		req.GetFromShardLeader(),
		channel,
		req.GetSegmentIDs(),
	))

	resp, err := segments.ReduceSearchResults(ctx, results, req.Req.GetNq(), req.Req.GetTopk(), req.Req.GetMetricType())
	if err != nil {
		return nil, err
	}

	tr.CtxElapse(ctx, fmt.Sprintf("do search with channel done , vChannel = %s, segmentIDs = %v",
		channel,
		req.GetSegmentIDs(),
	))

	// update metric to prometheus
	latency := tr.ElapseSpan()
	metrics.QueryNodeSQReqLatency.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), metrics.SearchLabel, metrics.Leader).Observe(float64(latency.Milliseconds()))
	metrics.QueryNodeSQCount.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), metrics.SearchLabel, metrics.SuccessLabel, metrics.Leader).Inc()
	metrics.QueryNodeSearchNQ.WithLabelValues(fmt.Sprint(paramtable.GetNodeID())).Observe(float64(req.Req.GetNq()))
	metrics.QueryNodeSearchTopK.WithLabelValues(fmt.Sprint(paramtable.GetNodeID())).Observe(float64(req.Req.GetTopk()))

	return resp, nil
}

func (node *QueryNode) getChannelStatistics(ctx context.Context, req *querypb.GetStatisticsRequest, channel string) (*internalpb.GetStatisticsResponse, error) {
	log := log.Ctx(ctx).With(
		zap.Int64("collectionID", req.Req.GetCollectionID()),
		zap.String("channel", channel),
		zap.String("scope", req.GetScope().String()),
	)

	resp := &internalpb.GetStatisticsResponse{}

	if req.GetFromShardLeader() {
		var (
			results      []segments.SegmentStats
			readSegments []segments.Segment
			err          error
		)

		switch req.GetScope() {
		case querypb.DataScope_Historical:
			results, readSegments, err = segments.StatisticsHistorical(ctx, node.manager, req.Req.GetCollectionID(), req.Req.GetPartitionIDs(), req.GetSegmentIDs())
		case querypb.DataScope_Streaming:
			results, readSegments, err = segments.StatisticStreaming(ctx, node.manager, req.Req.GetCollectionID(), req.Req.GetPartitionIDs(), req.GetSegmentIDs())
		}

		if err != nil {
			log.Warn("get segments statistics failed", zap.Error(err))
			return nil, err
		}
		defer node.manager.Segment.Unpin(readSegments)
		return segmentStatsResponse(results), nil
	}

	sd, ok := node.delegators.Get(channel)
	if !ok {
		err := merr.WrapErrChannelNotFound(channel, "failed to get channel statistics")
		log.Warn("GetStatistics failed, failed to get query shard delegator", zap.Error(err))
		resp.Status = merr.Status(err)
		return resp, nil
	}

	results, err := sd.GetStatistics(ctx, req)
	if err != nil {
		log.Warn("failed to get statistics from delegator", zap.Error(err))
		resp.Status = merr.Status(err)
		return resp, nil
	}
	resp, err = reduceStatisticResponse(results)
	if err != nil {
		log.Warn("failed to reduce channel statistics", zap.Error(err))
		resp.Status = merr.Status(err)
		return resp, nil
	}

	return resp, nil
}

func segmentStatsResponse(segStats []segments.SegmentStats) *internalpb.GetStatisticsResponse {
	var totalRowNum int64
	for _, stats := range segStats {
		totalRowNum += stats.RowCount
	}

	resultMap := make(map[string]string)
	resultMap["row_count"] = strconv.FormatInt(totalRowNum, 10)

	ret := &internalpb.GetStatisticsResponse{
		Status: merr.Success(),
		Stats:  funcutil.Map2KeyValuePair(resultMap),
	}
	return ret
}

func reduceStatisticResponse(results []*internalpb.GetStatisticsResponse) (*internalpb.GetStatisticsResponse, error) {
	mergedResults := map[string]interface{}{
		"row_count": int64(0),
	}
	fieldMethod := map[string]func(string) error{
		"row_count": func(str string) error {
			count, err := strconv.ParseInt(str, 10, 64)
			if err != nil {
				return err
			}
			mergedResults["row_count"] = mergedResults["row_count"].(int64) + count
			return nil
		},
	}

	for _, partialResult := range results {
		for _, pair := range partialResult.Stats {
			fn, ok := fieldMethod[pair.Key]
			if !ok {
				return nil, fmt.Errorf("unknown statistic field: %s", pair.Key)
			}
			if err := fn(pair.Value); err != nil {
				return nil, err
			}
		}
	}

	stringMap := make(map[string]string)
	for k, v := range mergedResults {
		stringMap[k] = fmt.Sprint(v)
	}

	ret := &internalpb.GetStatisticsResponse{
		Status: merr.Success(),
		Stats:  funcutil.Map2KeyValuePair(stringMap),
	}
	return ret, nil
}
