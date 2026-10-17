package blockvol

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
)

const (
	quiesced   = orchestrator.BlockVolumeConsistency_BLOCK_VOLUME_CONSISTENCY_FILESYSTEM_QUIESCED
	paired     = orchestrator.BlockVolumeConsistency_BLOCK_VOLUME_CONSISTENCY_PAIRED_MEMORY
	coldBoot   = orchestrator.BlockVolumeCloneMode_BLOCK_VOLUME_CLONE_MODE_COLD_BOOT
	memRestore = orchestrator.BlockVolumeCloneMode_BLOCK_VOLUME_CLONE_MODE_MEMORY_RESTORE
)

type grpcFixture struct {
	srv    *GRPCServer
	svc    *Service
	runner *fakeRunner
	live   *fakeLiveness
}

func newGRPCFixture(t *testing.T, tiers ...uint64) grpcFixture {
	t.Helper()

	svc, runner, live := newFakeService(t, smallWatermarks())

	return grpcFixture{srv: NewGRPCServer(svc, svc.Layout().Root(), tiers), svc: svc, runner: runner, live: live}
}

func refProto(v VolumeRef) *orchestrator.BlockVolumeRef {
	return &orchestrator.BlockVolumeRef{TeamId: v.Team.String(), VolumeId: v.Volume.String()}
}

func (f grpcFixture) create(t *testing.T, vol VolumeRef) *orchestrator.CreateBlockVolumeResponse {
	t.Helper()

	resp, err := f.srv.CreateBlockVolume(t.Context(), &orchestrator.CreateBlockVolumeRequest{
		OperationId:   NewID().String(),
		Volume:        refProto(vol),
		IncarnationId: NewID().String(),
		RevisionId:    NewID().String(),
		CapacityBytes: fakeCapacity,
	})
	require.NoError(t, err)

	return resp
}

func requireCode(t *testing.T, err error, code codes.Code) *status.Status {
	t.Helper()

	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok, "not a status error: %v", err)
	require.Equal(t, code, st.Code(), st.Message())

	return st
}

func errorInfo(t *testing.T, st *status.Status) *errdetails.ErrorInfo {
	t.Helper()

	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			return info
		}
	}

	t.Fatalf("no ErrorInfo in %v", st.Details())

	return nil
}

func TestGRPCCreateBlockVolume(t *testing.T) {
	t.Parallel()

	f := newGRPCFixture(t)
	vol, _ := newVolume()

	req := &orchestrator.CreateBlockVolumeRequest{
		OperationId:   NewID().String(),
		Volume:        refProto(vol),
		IncarnationId: NewID().String(),
		RevisionId:    NewID().String(),
		CapacityBytes: fakeCapacity,
	}

	resp, err := f.srv.CreateBlockVolume(t.Context(), req)
	require.NoError(t, err)

	inc, rev := resp.GetIncarnation(), resp.GetRevision()
	assert.Equal(t, req.GetIncarnationId(), inc.GetIncarnationId())
	assert.Empty(t, inc.GetParentRevisionId())
	assert.Equal(t, "teams/"+vol.Team.String()+"/"+vol.Volume.String()+"/inc/"+req.GetIncarnationId(), inc.GetSubvolPath(),
		"the path is relative to the node root, never a host path")
	assert.EqualValues(t, fakeCapacity, inc.GetCapacityBytes())
	assert.Equal(t, req.GetRevisionId(), rev.GetRevisionId())
	assert.Equal(t, req.GetOperationId(), rev.GetSourceOperationId())
	assert.Equal(t, inc.GetFsUuid(), rev.GetFsUuid())
	assert.Equal(t, inc.GetSubvolUuid(), rev.GetParentSubvolUuid())
	assert.EqualValues(t, fakeCapacity, rev.GetSizeBytes())
	assert.Equal(t, quiesced, rev.GetConsistency(), "revision 0 is a never-mounted image")
	assert.NotNil(t, rev.GetSealedAt())

	replay, err := f.srv.CreateBlockVolume(t.Context(), req)
	require.NoError(t, err)
	assert.Equal(t, resp.GetRevision().GetSubvolUuid(), replay.GetRevision().GetSubvolUuid())
	assert.Equal(t, 1, f.runner.count("mkfs.ext4"), "a replay does no filesystem work")

	changed := proto.CloneOf(req)
	changed.RevisionId = NewID().String()
	_, err = f.srv.CreateBlockVolume(t.Context(), changed)
	requireCode(t, err, codes.AlreadyExists)
}

func TestGRPCCreateBlockVolumeValidation(t *testing.T) {
	t.Parallel()

	f := newGRPCFixture(t, 2*fakeCapacity)
	vol, _ := newVolume()

	valid := func() *orchestrator.CreateBlockVolumeRequest {
		return &orchestrator.CreateBlockVolumeRequest{
			OperationId:   NewID().String(),
			Volume:        refProto(vol),
			IncarnationId: NewID().String(),
			RevisionId:    NewID().String(),
			CapacityBytes: 2 * fakeCapacity,
		}
	}

	for name, mutate := range map[string]func(*orchestrator.CreateBlockVolumeRequest){
		"missing operation":   func(r *orchestrator.CreateBlockVolumeRequest) { r.OperationId = "" },
		"non-canonical team":  func(r *orchestrator.CreateBlockVolumeRequest) { r.Volume.TeamId = "../etc" },
		"missing volume":      func(r *orchestrator.CreateBlockVolumeRequest) { r.Volume = nil },
		"uppercase revision":  func(r *orchestrator.CreateBlockVolumeRequest) { r.RevisionId = "ABCDEF00-0000-0000-0000-000000000000" },
		"zero capacity":       func(r *orchestrator.CreateBlockVolumeRequest) { r.CapacityBytes = 0 },
		"capacity off a tier": func(r *orchestrator.CreateBlockVolumeRequest) { r.CapacityBytes = fakeCapacity },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			req := valid()
			mutate(req)
			_, err := f.srv.CreateBlockVolume(t.Context(), req)
			requireCode(t, err, codes.InvalidArgument)
		})
	}

	_, err := f.srv.CreateBlockVolume(t.Context(), valid())
	require.NoError(t, err, "a tier capacity is accepted")
}

func TestGRPCCreateBlockVolumeBelowWatermark(t *testing.T) {
	t.Parallel()

	svc, _, _ := newFakeService(t, Watermarks{MinDataFree: 1 << 50, MinMetadataFree: 1})
	srv := NewGRPCServer(svc, svc.Layout().Root(), nil)
	vol, _ := newVolume()

	_, err := srv.CreateBlockVolume(t.Context(), &orchestrator.CreateBlockVolumeRequest{
		OperationId: NewID().String(), Volume: refProto(vol),
		IncarnationId: NewID().String(), RevisionId: NewID().String(), CapacityBytes: fakeCapacity,
	})
	st := requireCode(t, err, codes.ResourceExhausted)
	assert.Equal(t, "CAPACITY", errorInfo(t, st).GetReason())
}

func TestGRPCCloneBlockVolumeModes(t *testing.T) {
	t.Parallel()

	f := newGRPCFixture(t)
	vol, _ := newVolume()
	created := f.create(t, vol)
	srcRev := created.GetRevision().GetRevisionId()

	clone := func(consistency orchestrator.BlockVolumeConsistency, mode orchestrator.BlockVolumeCloneMode) (*orchestrator.CloneBlockVolumeResponse, error) {
		return f.srv.CloneBlockVolume(t.Context(), &orchestrator.CloneBlockVolumeRequest{
			OperationId:       NewID().String(),
			Volume:            refProto(vol),
			SourceRevisionId:  srcRev,
			SourceConsistency: consistency,
			Mode:              mode,
			IncarnationId:     NewID().String(),
		})
	}

	cold, err := clone(quiesced, coldBoot)
	require.NoError(t, err)
	assert.NotEqual(t, created.GetRevision().GetFsUuid(), cold.GetIncarnation().GetFsUuid(), "a cold-boot fork gets a fresh filesystem UUID")
	assert.Equal(t, srcRev, cold.GetIncarnation().GetParentRevisionId())
	assert.Equal(t, 1, f.runner.count("tune2fs"))

	// The node trusts the recorded consistency it is given (the revision on
	// disk does not carry one) and derives UUID handling from it.
	mem, err := clone(paired, memRestore)
	require.NoError(t, err)
	assert.Equal(t, created.GetRevision().GetFsUuid(), mem.GetIncarnation().GetFsUuid(), "a memory fork keeps the UUID the restored kernel has mounted")
	assert.Equal(t, 1, f.runner.count("tune2fs"))

	for _, tc := range []struct {
		consistency orchestrator.BlockVolumeConsistency
		mode        orchestrator.BlockVolumeCloneMode
	}{
		{paired, coldBoot},
		{quiesced, memRestore},
		{quiesced, orchestrator.BlockVolumeCloneMode_BLOCK_VOLUME_CLONE_MODE_UNSPECIFIED},
		{orchestrator.BlockVolumeConsistency_BLOCK_VOLUME_CONSISTENCY_UNSPECIFIED, coldBoot},
	} {
		_, err := clone(tc.consistency, tc.mode)
		requireCode(t, err, codes.InvalidArgument)
	}

	// Into another volume of the same team.
	target := VolumeRef{Team: vol.Team, Volume: NewID()}
	into, err := f.srv.CloneBlockVolume(t.Context(), &orchestrator.CloneBlockVolumeRequest{
		OperationId: NewID().String(), Volume: refProto(target), SourceVolume: refProto(vol),
		SourceRevisionId: srcRev, SourceConsistency: quiesced, Mode: coldBoot, IncarnationId: NewID().String(),
	})
	require.NoError(t, err)
	assert.Contains(t, into.GetIncarnation().GetSubvolPath(), target.Volume.String())

	_, err = f.srv.CloneBlockVolume(t.Context(), &orchestrator.CloneBlockVolumeRequest{
		OperationId: NewID().String(), Volume: refProto(VolumeRef{Team: NewID(), Volume: NewID()}), SourceVolume: refProto(vol),
		SourceRevisionId: srcRev, SourceConsistency: quiesced, Mode: coldBoot, IncarnationId: NewID().String(),
	})
	requireCode(t, err, codes.InvalidArgument)

	_, err = f.srv.CloneBlockVolume(t.Context(), &orchestrator.CloneBlockVolumeRequest{
		OperationId: NewID().String(), Volume: refProto(vol), SourceRevisionId: NewID().String(),
		SourceConsistency: quiesced, Mode: coldBoot, IncarnationId: NewID().String(),
	})
	requireCode(t, err, codes.NotFound)
}

func TestGRPCSealBlockVolume(t *testing.T) {
	t.Parallel()

	f := newGRPCFixture(t)
	vol, _ := newVolume()
	created := f.create(t, vol)
	incID := created.GetIncarnation().GetIncarnationId()

	seal := func(execution string, consistency orchestrator.BlockVolumeConsistency) (*orchestrator.SealBlockVolumeResponse, error) {
		return f.srv.SealBlockVolume(t.Context(), &orchestrator.SealBlockVolumeRequest{
			OperationId: NewID().String(), Volume: refProto(vol), IncarnationId: incID,
			RevisionId: NewID().String(), ExecutionId: execution, Consistency: consistency,
		})
	}

	_, err := seal("", orchestrator.BlockVolumeConsistency_BLOCK_VOLUME_CONSISTENCY_UNSPECIFIED)
	requireCode(t, err, codes.InvalidArgument)

	_, err = seal("exec-1", paired)
	requireCode(t, err, codes.PermissionDenied)

	f.live.spawn(42, 4200)
	_, err = f.svc.Bind(t.Context(), BindRequest{
		Incarnation: IncarnationRef{VolumeRef: vol, Incarnation: MustID(incID)},
		SandboxID:   "sbx", ExecutionID: "exec-1", FCPid: 42,
	})
	require.NoError(t, err)

	resp, err := seal("exec-1", paired)
	require.NoError(t, err)
	assert.Equal(t, paired, resp.GetRevision().GetConsistency())
	assert.Equal(t, incID, resp.GetRevision().GetSourceIncarnationId())

	_, err = f.srv.SealBlockVolume(t.Context(), &orchestrator.SealBlockVolumeRequest{
		OperationId: NewID().String(), Volume: refProto(vol), IncarnationId: NewID().String(),
		RevisionId: NewID().String(), Consistency: quiesced,
	})
	requireCode(t, err, codes.NotFound)
}

func TestGRPCDeleteAndReconcileBlockVolume(t *testing.T) {
	t.Parallel()

	f := newGRPCFixture(t)
	ctx := t.Context()
	vol, _ := newVolume()
	other, _ := newVolume()
	created := f.create(t, vol)
	f.create(t, other)

	incID := created.GetIncarnation().GetIncarnationId()
	revID := created.GetRevision().GetRevisionId()

	clean, err := f.srv.ReconcileBlockVolume(ctx, &orchestrator.ReconcileBlockVolumeRequest{
		Volume: refProto(vol), IncarnationIds: []string{incID}, RevisionIds: []string{revID},
	})
	require.NoError(t, err)
	assert.True(t, clean.GetClean(), "%v", clean)
	assert.Empty(t, clean.GetUnexpectedIncarnationIds(), "another volume's images are not reported")
	assert.Len(t, clean.GetStagingOperationIds(), 2)

	f.live.spawn(7, 70)
	_, err = f.svc.Bind(ctx, BindRequest{Incarnation: IncarnationRef{VolumeRef: vol, Incarnation: MustID(incID)}, SandboxID: "sbx", ExecutionID: "e", FCPid: 7})
	require.NoError(t, err)

	held, err := f.srv.ReconcileBlockVolume(ctx, &orchestrator.ReconcileBlockVolumeRequest{
		Volume: refProto(vol), RevisionIds: []string{revID},
	})
	require.NoError(t, err)
	assert.False(t, held.GetClean())
	assert.Equal(t, []string{incID}, held.GetUnexpectedIncarnationIds())
	require.Len(t, held.GetUnexpectedHolders(), 1, "the holder of an unexpected incarnation of this volume is reported")
	assert.Equal(t, "e", held.GetUnexpectedHolders()[0].GetExecutionId())

	del := &orchestrator.DeleteBlockVolumeRequest{
		OperationId: NewID().String(), Volume: refProto(vol),
		RevisionIds: []string{revID}, IncarnationIds: []string{incID},
	}
	_, err = f.srv.DeleteBlockVolume(ctx, del)
	st := requireCode(t, err, codes.FailedPrecondition)
	assert.Equal(t, "INCARNATION_HELD", errorInfo(t, st).GetReason())

	f.live.kill(7)
	require.NoError(t, f.svc.Release(ctx, ReleaseRequest{Incarnation: MustID(incID), ExecutionID: "e"}))

	_, err = f.srv.DeleteBlockVolume(ctx, del)
	require.NoError(t, err)
	_, err = f.srv.DeleteBlockVolume(ctx, del)
	require.NoError(t, err, "delete is idempotent")

	gone, err := f.srv.ReconcileBlockVolume(ctx, &orchestrator.ReconcileBlockVolumeRequest{Volume: refProto(vol)})
	require.NoError(t, err)
	assert.True(t, gone.GetClean())

	_, err = f.srv.ForgetBlockVolumeOperation(ctx, &orchestrator.ForgetBlockVolumeOperationRequest{OperationId: created.GetRevision().GetSourceOperationId()})
	require.NoError(t, err)

	after, err := f.srv.ReconcileBlockVolume(ctx, &orchestrator.ReconcileBlockVolumeRequest{Volume: refProto(vol)})
	require.NoError(t, err)
	assert.Len(t, after.GetStagingOperationIds(), 1, "only the other volume's create is left in staging")
}

func TestToStatus(t *testing.T) {
	t.Parallel()

	cmd := &CommandError{Name: "mkfs.ext4", Args: []string{"/secret/host/path"}, ExitCode: 1, Stderr: "disk on fire", Err: errors.New("exit status 1")}
	st := requireCode(t, toStatus(cmd), codes.Internal)
	assert.NotContains(t, st.Message(), "disk on fire", "stderr is never in the message")
	assert.NotContains(t, st.Message(), "/secret/host/path")
	assert.Equal(t, "disk on fire", errorInfo(t, st).GetMetadata()["stderr"])

	for err, code := range map[error]codes.Code{
		ErrInvalidID:                 codes.InvalidArgument,
		ErrReclaimEvidenceMismatch:   codes.InvalidArgument,
		ErrOperationConflict:         codes.AlreadyExists,
		ErrRevisionNotFound:          codes.NotFound,
		ErrIncarnationNotFound:       codes.NotFound,
		&CapacityError{Pool: "data"}: codes.ResourceExhausted,
		&HeldByError{}:               codes.FailedPrecondition,
		&BlockedError{}:              codes.FailedPrecondition,
		&StillRunningError{}:         codes.FailedPrecondition,
		ErrIncarnationHeld:           codes.FailedPrecondition,
		ErrHolderMismatch:            codes.PermissionDenied,
		context.DeadlineExceeded:     codes.DeadlineExceeded,
		errors.New("other"):          codes.Internal,
	} {
		requireCode(t, toStatus(err), code)
	}

	requireCode(t, toStatus(status.Error(codes.Aborted, "kept")), codes.Aborted)
	require.NoError(t, toStatus(nil))
}

// TestGRPCServiceRegistration round-trips through a real gRPC server, so the
// generated service descriptor and the server agree.
func TestGRPCServiceRegistration(t *testing.T) {
	t.Parallel()

	f := newGRPCFixture(t)

	lis := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	orchestrator.RegisterBlockVolumeServiceServer(server, f.srv)
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	client := orchestrator.NewBlockVolumeServiceClient(conn)
	vol, _ := newVolume()

	resp, err := client.CreateBlockVolume(t.Context(), &orchestrator.CreateBlockVolumeRequest{
		OperationId: NewID().String(), Volume: refProto(vol),
		IncarnationId: NewID().String(), RevisionId: NewID().String(), CapacityBytes: fakeCapacity,
	})
	require.NoError(t, err)
	assert.Equal(t, quiesced, resp.GetRevision().GetConsistency())

	_, err = client.SealBlockVolume(t.Context(), &orchestrator.SealBlockVolumeRequest{})
	requireCode(t, err, codes.InvalidArgument)
}
