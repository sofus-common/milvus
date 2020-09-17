package reader

import (
	"encoding/binary"
	"fmt"
	msgPb "github.com/czs007/suvlim/pkg/master/grpc/message"
	"github.com/stretchr/testify/assert"
	"math"
	"testing"
)

func TestIndex_BuildIndex(t *testing.T) {
	// 1. Construct node, collection, partition and segment
	node := NewQueryNode(0, 0)
	var collection = node.NewCollection("collection0", "fake schema")
	var partition = collection.NewPartition("partition0")
	var segment = partition.NewSegment(0)

	// 2. Create ids and timestamps
	ids := make([]int64, 0)
	timestamps := make([]uint64, 0)

	// 3. Create records, use schema below:
	// schema_tmp->AddField("fakeVec", DataType::VECTOR_FLOAT, 16);
	// schema_tmp->AddField("age", DataType::INT32);
	const DIM = 16
	const N = 10000
	var vec = [DIM]float32{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	var rawData []byte
	for _, ele := range vec {
		buf := make([]byte, 4)
		binary.LittleEndian.PutUint32(buf, math.Float32bits(ele))
		rawData = append(rawData, buf...)
	}
	bs := make([]byte, 4)
	binary.LittleEndian.PutUint32(bs, 1)
	rawData = append(rawData, bs...)
	var records [][]byte
	for i := 0; i < N; i++ {
		ids = append(ids, int64(i))
		timestamps = append(timestamps, uint64(i))
		records = append(records, rawData)
	}

	// 4. Do PreInsert
	var offset = segment.SegmentPreInsert(N)
	assert.GreaterOrEqual(t, offset, int64(0))

	// 5. Do Insert
	var err = segment.SegmentInsert(offset, &ids, &timestamps, &records)
	assert.NoError(t, err)

	// 6. Close segment, and build index
	err = segment.Close()
	assert.NoError(t, err)

	// 7. Do search
	var queryJson = "{\"field_name\":\"fakevec\",\"num_queries\":1,\"topK\":10}"
	var queryRawData = make([]float32, 0)
	for i := 0; i < 16; i++ {
		queryRawData = append(queryRawData, float32(i))
	}
	var vectorRecord = msgPb.VectorRowRecord{
		FloatData: queryRawData,
	}
	var searchRes, searchErr = segment.SegmentSearch(queryJson, timestamps[N/2], &vectorRecord)
	assert.NoError(t, searchErr)
	fmt.Println(searchRes)

	// 8. Destruct node, collection, and segment
	partition.DeleteSegment(segment)
	collection.DeletePartition(partition)
	node.DeleteCollection(collection)
}
