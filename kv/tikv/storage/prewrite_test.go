package storage

import (
	"github.com/pingcap-incubator/tinykv/kv/tikv/inner_server"
	"github.com/pingcap-incubator/tinykv/kv/tikv/storage/commands"
	"github.com/pingcap-incubator/tinykv/kv/tikv/storage/exec"
	"github.com/pingcap-incubator/tinykv/proto/pkg/kvrpcpb"
	"github.com/stretchr/testify/assert"
	"testing"
)

// TestEmptyPrewrite tests that a Prewrite with no mutations succeeds and changes nothing.
func TestEmptyPrewrite(t *testing.T) {
	mem := inner_server.NewMemInnerServer()
	sched := exec.NewSeqScheduler(mem)

	builder := NewReqBuilder()
	cmd := commands.NewPrewrite(builder.request())
	resp := run(t, sched, &cmd)[0].(*kvrpcpb.PrewriteResponse)
	assert.Empty(t, resp.Errors)
	assert.Nil(t, resp.RegionError)
	assert.Equal(t, 0, mem.Len(inner_server.CfDefault))
}

// TestSinglePrewrite tests a prewrite with one write, it should succeed, we test all the expected values.
func TestSinglePrewrite(t *testing.T) {
	mem := inner_server.NewMemInnerServer()
	sched := exec.NewSeqScheduler(mem)

	builder := NewReqBuilder()
	cmd := commands.NewPrewrite(builder.request(mutation(3, []byte{42}, kvrpcpb.Op_Put)))
	resp := run(t, sched, &cmd)[0].(*kvrpcpb.PrewriteResponse)
	assert.Empty(t, resp.Errors)
	assert.Nil(t, resp.RegionError)
	assert.Equal(t, mem.Len(inner_server.CfDefault), 1)
	assert.Equal(t, mem.Len(inner_server.CfLock), 1)

	assert.Equal(t, []byte{42}, mem.Get(inner_server.CfDefault, 3, 0, 0, 0, 0, 0, 0, 0, 100))
	assert.Equal(t, []byte{1, 0, 0, 0, 0, 0, 0, 0, 100}, mem.Get(inner_server.CfLock, 3))
}

// TestBadWritePrewrites tests that two prewrites to the same key causes a lock error.
func TestBadWritePrewrites(t *testing.T) {
	mem := inner_server.NewMemInnerServer()
	sched := exec.NewSeqScheduler(mem)

	builder := NewReqBuilder()
	cmd := commands.NewPrewrite(builder.request(mutation(3, []byte{42}, kvrpcpb.Op_Put)))
	cmd2 := commands.NewPrewrite(builder.request(mutation(3, []byte{53}, kvrpcpb.Op_Put)))
	resps := run(t, sched, &cmd, &cmd2)

	assert.Empty(t, resps[0].(*kvrpcpb.PrewriteResponse).Errors)
	assert.Nil(t, resps[0].(*kvrpcpb.PrewriteResponse).RegionError)
	assert.Equal(t, len(resps[1].(*kvrpcpb.PrewriteResponse).Errors), 1)
	assert.Nil(t, resps[1].(*kvrpcpb.PrewriteResponse).RegionError)

	assert.Equal(t, mem.Len(inner_server.CfDefault), 1)
	assert.Equal(t, mem.Len(inner_server.CfLock), 1)
	assert.Equal(t, []byte{42}, mem.Get(inner_server.CfDefault, 3, 0, 0, 0, 0, 0, 0, 0, 100))
	assert.Equal(t, []byte{1, 0, 0, 0, 0, 0, 0, 0, 100}, mem.Get(inner_server.CfLock, 3))
}

// TestMultiplePrewrites tests that multiple prewrites to different keys succeeds.
func TestMultiplePrewrites(t *testing.T) {
	mem := inner_server.NewMemInnerServer()
	sched := exec.NewSeqScheduler(mem)

	builder := NewReqBuilder()
	cmd := commands.NewPrewrite(builder.request(mutation(3, []byte{42}, kvrpcpb.Op_Put)))
	cmd2 := commands.NewPrewrite(builder.request(mutation(4, []byte{53}, kvrpcpb.Op_Put)))
	resps := run(t, sched, &cmd, &cmd2)

	assert.Empty(t, resps[0].(*kvrpcpb.PrewriteResponse).Errors)
	assert.Nil(t, resps[0].(*kvrpcpb.PrewriteResponse).RegionError)
	assert.Empty(t, resps[1].(*kvrpcpb.PrewriteResponse).Errors)
	assert.Nil(t, resps[1].(*kvrpcpb.PrewriteResponse).RegionError)

	assert.Equal(t, mem.Len(inner_server.CfDefault), 2)
	assert.Equal(t, mem.Len(inner_server.CfLock), 2)
	assert.Equal(t, []byte{42}, mem.Get(inner_server.CfDefault, 3, 0, 0, 0, 0, 0, 0, 0, 100))
	assert.Equal(t, []byte{1, 0, 0, 0, 0, 0, 0, 0, 100}, mem.Get(inner_server.CfLock, 3))
	assert.Equal(t, []byte{53}, mem.Get(inner_server.CfDefault, 4, 0, 0, 0, 0, 0, 0, 0, 101))
	assert.Equal(t, []byte{1, 0, 0, 0, 0, 0, 0, 0, 101}, mem.Get(inner_server.CfLock, 4))
}

// TestPrewriteOverwrite tests that two writes in the same prewrite succeed and we see the second write.
func TestPrewriteOverwrite(t *testing.T) {
	mem := inner_server.NewMemInnerServer()
	sched := exec.NewSeqScheduler(mem)

	builder := NewReqBuilder()
	cmd := commands.NewPrewrite(builder.request(mutation(3, []byte{42}, kvrpcpb.Op_Put), mutation(3, []byte{45}, kvrpcpb.Op_Put)))
	resp := run(t, sched, &cmd)[0].(*kvrpcpb.PrewriteResponse)
	assert.Empty(t, resp.Errors)
	assert.Nil(t, resp.RegionError)
	assert.Equal(t, mem.Len(inner_server.CfDefault), 1)
	assert.Equal(t, mem.Len(inner_server.CfLock), 1)

	assert.Equal(t, []byte{45}, mem.Get(inner_server.CfDefault, 3, 0, 0, 0, 0, 0, 0, 0, 100))
	assert.Equal(t, []byte{1, 0, 0, 0, 0, 0, 0, 0, 100}, mem.Get(inner_server.CfLock, 3))
}

// TestPrewriteMultiple tests that a prewrite with multiple mutations succeeds.
func TestPrewriteMultiple(t *testing.T) {
	mem := inner_server.NewMemInnerServer()
	sched := exec.NewSeqScheduler(mem)

	builder := NewReqBuilder()
	cmd := commands.NewPrewrite(builder.request(
		mutation(3, []byte{42}, kvrpcpb.Op_Put),
		mutation(4, []byte{43}, kvrpcpb.Op_Put),
		mutation(5, []byte{44}, kvrpcpb.Op_Insert),
		mutation(4, nil, kvrpcpb.Op_Del),
		mutation(4, []byte{1, 3, 5}, kvrpcpb.Op_Insert),
		mutation(255, []byte{45}, kvrpcpb.Op_Put),
	))
	resp := run(t, sched, &cmd)[0].(*kvrpcpb.PrewriteResponse)
	assert.Empty(t, resp.Errors)
	assert.Nil(t, resp.RegionError)
	assert.Equal(t, mem.Len(inner_server.CfDefault), 4)
	assert.Equal(t, mem.Len(inner_server.CfLock), 4)

	assert.Equal(t, []byte{1, 3, 5}, mem.Get(inner_server.CfDefault, 4, 0, 0, 0, 0, 0, 0, 0, 100))
}

type requestBuilder struct {
	nextTS uint64
}

func NewReqBuilder() requestBuilder {
	return requestBuilder{
		nextTS: 100,
	}
}

func (builder *requestBuilder) request(muts ...*kvrpcpb.Mutation) *kvrpcpb.PrewriteRequest {
	var req kvrpcpb.PrewriteRequest
	req.PrimaryLock = []byte{1}
	req.StartVersion = builder.nextTS
	builder.nextTS++
	req.Mutations = muts
	return &req
}

func mutation(key byte, value []byte, op kvrpcpb.Op) *kvrpcpb.Mutation {
	var mut kvrpcpb.Mutation
	mut.Key = []byte{key}
	mut.Value = value
	mut.Op = op
	return &mut
}
