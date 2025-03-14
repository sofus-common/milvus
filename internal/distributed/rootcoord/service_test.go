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

package grpcrootcoord

import (
	"context"
	"fmt"
	"math/rand"
	"path"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/assert"
	"github.com/tikv/client-go/v2/txnkv"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/milvuspb"
	"github.com/milvus-io/milvus/internal/rootcoord"
	"github.com/milvus-io/milvus/internal/types"
	"github.com/milvus-io/milvus/internal/util/sessionutil"
	"github.com/milvus-io/milvus/pkg/util/etcd"
	"github.com/milvus-io/milvus/pkg/util/merr"
	"github.com/milvus-io/milvus/pkg/util/paramtable"
	"github.com/milvus-io/milvus/pkg/util/tikv"
)

type mockCore struct {
	types.RootCoordComponent
}

func (m *mockCore) CreateDatabase(ctx context.Context, request *milvuspb.CreateDatabaseRequest) (*commonpb.Status, error) {
	return &commonpb.Status{ErrorCode: commonpb.ErrorCode_Success}, nil
}

func (m *mockCore) DropDatabase(ctx context.Context, request *milvuspb.DropDatabaseRequest) (*commonpb.Status, error) {
	return &commonpb.Status{ErrorCode: commonpb.ErrorCode_Success}, nil
}

func (m *mockCore) ListDatabases(ctx context.Context, request *milvuspb.ListDatabasesRequest) (*milvuspb.ListDatabasesResponse, error) {
	return &milvuspb.ListDatabasesResponse{
		Status: &commonpb.Status{ErrorCode: commonpb.ErrorCode_Success},
	}, nil
}

func (m *mockCore) RenameCollection(ctx context.Context, request *milvuspb.RenameCollectionRequest) (*commonpb.Status, error) {
	return &commonpb.Status{ErrorCode: commonpb.ErrorCode_Success}, nil
}

func (m *mockCore) CheckHealth(ctx context.Context, req *milvuspb.CheckHealthRequest) (*milvuspb.CheckHealthResponse, error) {
	return &milvuspb.CheckHealthResponse{
		IsHealthy: true,
	}, nil
}

func (m *mockCore) UpdateStateCode(commonpb.StateCode) {
}

func (m *mockCore) SetAddress(address string) {
}

func (m *mockCore) SetEtcdClient(etcdClient *clientv3.Client) {
}

func (m *mockCore) SetTiKVClient(client *txnkv.Client) {
}

func (m *mockCore) SetDataCoordClient(client types.DataCoordClient) error {
	return nil
}

func (m *mockCore) SetQueryCoordClient(client types.QueryCoordClient) error {
	return nil
}

func (m *mockCore) SetProxyCreator(func(ctx context.Context, addr string, nodeID int64) (types.ProxyClient, error)) {
}

func (m *mockCore) Register() error {
	return nil
}

func (m *mockCore) Init() error {
	return nil
}

func (m *mockCore) Start() error {
	return nil
}

func (m *mockCore) Stop() error {
	return fmt.Errorf("stop error")
}

type mockDataCoord struct {
	types.DataCoordClient
}

func (m *mockDataCoord) Close() error {
	return nil
}

func (m *mockDataCoord) GetComponentStates(ctx context.Context, req *milvuspb.GetComponentStatesRequest, opts ...grpc.CallOption) (*milvuspb.ComponentStates, error) {
	return &milvuspb.ComponentStates{
		State: &milvuspb.ComponentInfo{
			StateCode: commonpb.StateCode_Healthy,
		},
		Status: merr.Success(),
		SubcomponentStates: []*milvuspb.ComponentInfo{
			{
				StateCode: commonpb.StateCode_Healthy,
			},
		},
	}, nil
}

func (m *mockDataCoord) Stop() error {
	return fmt.Errorf("stop error")
}

type mockQueryCoord struct {
	types.QueryCoordClient
	initErr  error
	startErr error
}

func (m *mockQueryCoord) Close() error {
	return fmt.Errorf("stop error")
}

func TestRun(t *testing.T) {
	paramtable.Init()
	parameters := []string{"tikv", "etcd"}
	for _, v := range parameters {
		paramtable.Get().Save(paramtable.Get().MetaStoreCfg.MetaStoreType.Key, v)
		ctx, cancel := context.WithCancel(context.Background())
		getTiKVClient = func(cfg *paramtable.TiKVConfig) (*txnkv.Client, error) {
			return tikv.SetupLocalTxn(), nil
		}
		defer func() {
			getTiKVClient = tikv.GetTiKVClient
		}()
		svr := Server{
			rootCoord:   &mockCore{},
			ctx:         ctx,
			cancel:      cancel,
			grpcErrChan: make(chan error),
		}
		rcServerConfig := &paramtable.Get().RootCoordGrpcServerCfg
		paramtable.Get().Save(rcServerConfig.Port.Key, "1000000")
		err := svr.Run()
		assert.Error(t, err)
		assert.EqualError(t, err, "listen tcp: address 1000000: invalid port")

		svr.newDataCoordClient = func(string, *clientv3.Client) types.DataCoordClient {
			return &mockDataCoord{}
		}
		svr.newQueryCoordClient = func(string, *clientv3.Client) types.QueryCoordClient {
			return &mockQueryCoord{}
		}

		paramtable.Get().Save(rcServerConfig.Port.Key, fmt.Sprintf("%d", rand.Int()%100+10000))
		etcdConfig := &paramtable.Get().EtcdCfg

		rand.Seed(time.Now().UnixNano())
		randVal := rand.Int()
		rootPath := fmt.Sprintf("/%d/test", randVal)
		rootcoord.Params.Save("etcd.rootPath", rootPath)

		etcdCli, err := etcd.GetEtcdClient(
			etcdConfig.UseEmbedEtcd.GetAsBool(),
			etcdConfig.EtcdUseSSL.GetAsBool(),
			etcdConfig.Endpoints.GetAsStrings(),
			etcdConfig.EtcdTLSCert.GetValue(),
			etcdConfig.EtcdTLSKey.GetValue(),
			etcdConfig.EtcdTLSCACert.GetValue(),
			etcdConfig.EtcdTLSMinVersion.GetValue())
		assert.NoError(t, err)
		sessKey := path.Join(rootcoord.Params.EtcdCfg.MetaRootPath.GetValue(), sessionutil.DefaultServiceRoot)
		_, err = etcdCli.Delete(ctx, sessKey, clientv3.WithPrefix())
		assert.NoError(t, err)
		err = svr.Run()
		assert.NoError(t, err)

		t.Run("CheckHealth", func(t *testing.T) {
			ret, err := svr.CheckHealth(ctx, nil)
			assert.NoError(t, err)
			assert.Equal(t, true, ret.IsHealthy)
		})

		t.Run("RenameCollection", func(t *testing.T) {
			_, err := svr.RenameCollection(ctx, nil)
			assert.NoError(t, err)
		})

		t.Run("CreateDatabase", func(t *testing.T) {
			ret, err := svr.CreateDatabase(ctx, nil)
			assert.Nil(t, err)
			assert.Equal(t, commonpb.ErrorCode_Success, ret.ErrorCode)
		})

		t.Run("DropDatabase", func(t *testing.T) {
			ret, err := svr.DropDatabase(ctx, nil)
			assert.Nil(t, err)
			assert.Equal(t, commonpb.ErrorCode_Success, ret.ErrorCode)
		})

		t.Run("ListDatabases", func(t *testing.T) {
			ret, err := svr.ListDatabases(ctx, nil)
			assert.Nil(t, err)
			assert.Equal(t, commonpb.ErrorCode_Success, ret.GetStatus().GetErrorCode())
		})
		err = svr.Stop()
		assert.NoError(t, err)
	}
}

func TestServerRun_DataCoordClientInitErr(t *testing.T) {
	paramtable.Init()
	parameters := []string{"tikv", "etcd"}
	for _, v := range parameters {
		paramtable.Get().Save(paramtable.Get().MetaStoreCfg.MetaStoreType.Key, v)
		ctx := context.Background()
		getTiKVClient = func(cfg *paramtable.TiKVConfig) (*txnkv.Client, error) {
			return tikv.SetupLocalTxn(), nil
		}
		defer func() {
			getTiKVClient = tikv.GetTiKVClient
		}()
		server, err := NewServer(ctx, nil)
		assert.NoError(t, err)
		assert.NotNil(t, server)

		server.newDataCoordClient = func(string, *clientv3.Client) types.DataCoordClient {
			return &mockDataCoord{}
		}
		assert.Panics(t, func() { server.Run() })

		err = server.Stop()
		assert.NoError(t, err)
	}
}

func TestServerRun_DataCoordClientStartErr(t *testing.T) {
	paramtable.Init()
	parameters := []string{"tikv", "etcd"}
	for _, v := range parameters {
		paramtable.Get().Save(paramtable.Get().MetaStoreCfg.MetaStoreType.Key, v)
		ctx := context.Background()
		getTiKVClient = func(cfg *paramtable.TiKVConfig) (*txnkv.Client, error) {
			return tikv.SetupLocalTxn(), nil
		}
		defer func() {
			getTiKVClient = tikv.GetTiKVClient
		}()
		server, err := NewServer(ctx, nil)
		assert.NoError(t, err)
		assert.NotNil(t, server)

		server.newDataCoordClient = func(string, *clientv3.Client) types.DataCoordClient {
			return &mockDataCoord{}
		}
		assert.Panics(t, func() { server.Run() })

		err = server.Stop()
		assert.NoError(t, err)
	}
}

func TestServerRun_QueryCoordClientInitErr(t *testing.T) {
	paramtable.Init()
	parameters := []string{"tikv", "etcd"}
	for _, v := range parameters {
		paramtable.Get().Save(paramtable.Get().MetaStoreCfg.MetaStoreType.Key, v)
		ctx := context.Background()
		getTiKVClient = func(cfg *paramtable.TiKVConfig) (*txnkv.Client, error) {
			return tikv.SetupLocalTxn(), nil
		}
		defer func() {
			getTiKVClient = tikv.GetTiKVClient
		}()
		server, err := NewServer(ctx, nil)
		assert.NoError(t, err)
		assert.NotNil(t, server)

		server.newQueryCoordClient = func(string, *clientv3.Client) types.QueryCoordClient {
			return &mockQueryCoord{initErr: errors.New("mock querycoord init error")}
		}
		assert.Panics(t, func() { server.Run() })

		err = server.Stop()
		assert.NoError(t, err)
	}
}

func TestServer_QueryCoordClientStartErr(t *testing.T) {
	paramtable.Init()
	parameters := []string{"tikv", "etcd"}
	for _, v := range parameters {
		paramtable.Get().Save(paramtable.Get().MetaStoreCfg.MetaStoreType.Key, v)
		ctx := context.Background()
		getTiKVClient = func(cfg *paramtable.TiKVConfig) (*txnkv.Client, error) {
			return tikv.SetupLocalTxn(), nil
		}
		defer func() {
			getTiKVClient = tikv.GetTiKVClient
		}()
		server, err := NewServer(ctx, nil)
		assert.NoError(t, err)
		assert.NotNil(t, server)

		server.newQueryCoordClient = func(string, *clientv3.Client) types.QueryCoordClient {
			return &mockQueryCoord{startErr: errors.New("mock querycoord start error")}
		}
		assert.Panics(t, func() { server.Run() })

		err = server.Stop()
		assert.NoError(t, err)
	}
}
