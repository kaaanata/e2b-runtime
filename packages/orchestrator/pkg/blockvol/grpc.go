package blockvol

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
)

// errorDomain is the ErrorInfo domain of BlockVolumeService errors.
const errorDomain = "blockvol.e2b.dev"

// GRPCServer exposes a Backend as the orchestrator BlockVolumeService. It is
// the only way the control plane changes block volumes; it never serves
// volume content (that goes through the sandbox's envd).
type GRPCServer struct {
	orchestrator.UnimplementedBlockVolumeServiceServer

	backend Backend
	root    string
	tiers   []uint64
}

var _ orchestrator.BlockVolumeServiceServer = (*GRPCServer)(nil)

// NewGRPCServer serves backend, whose layout is rooted at root (used to report
// subvolume paths relative to it). capacityTiers, when non-empty, is the
// exhaustive set of capacities (bytes) CreateBlockVolume accepts.
func NewGRPCServer(backend Backend, root string, capacityTiers []uint64) *GRPCServer {
	return &GRPCServer{backend: backend, root: root, tiers: slices.Clone(capacityTiers)}
}

func (s *GRPCServer) CreateBlockVolume(ctx context.Context, req *orchestrator.CreateBlockVolumeRequest) (*orchestrator.CreateBlockVolumeResponse, error) {
	op, err := parseRequestID("operation_id", req.GetOperationId())
	if err != nil {
		return nil, err
	}

	vol, err := parseVolumeRef(req.GetVolume())
	if err != nil {
		return nil, err
	}

	incID, err := parseRequestID("incarnation_id", req.GetIncarnationId())
	if err != nil {
		return nil, err
	}

	revID, err := parseRequestID("revision_id", req.GetRevisionId())
	if err != nil {
		return nil, err
	}

	capacity := req.GetCapacityBytes()
	if capacity == 0 {
		return nil, status.Error(codes.InvalidArgument, "capacity_bytes must be positive")
	}

	if len(s.tiers) > 0 && !slices.Contains(s.tiers, capacity) {
		return nil, status.Errorf(codes.InvalidArgument, "capacity_bytes %d is not one of the node's capacity tiers", capacity)
	}

	res, err := s.backend.Create(ctx, op, IncarnationRef{VolumeRef: vol, Incarnation: incID}, revID, capacity)
	if err != nil {
		return nil, toStatus(err)
	}

	// A fresh mkfs that no guest has mounted is cold-bootable.
	res.Revision.Consistency = ConsistencyFilesystemQuiesced

	return &orchestrator.CreateBlockVolumeResponse{
		Incarnation: s.incarnationToProto(res.Incarnation),
		Revision:    revisionToProto(res.Revision),
	}, nil
}

func (s *GRPCServer) CloneBlockVolume(ctx context.Context, req *orchestrator.CloneBlockVolumeRequest) (*orchestrator.CloneBlockVolumeResponse, error) {
	op, err := parseRequestID("operation_id", req.GetOperationId())
	if err != nil {
		return nil, err
	}

	vol, err := parseVolumeRef(req.GetVolume())
	if err != nil {
		return nil, err
	}

	source := vol
	if req.GetSourceVolume() != nil {
		source, err = parseVolumeRef(req.GetSourceVolume())
		if err != nil {
			return nil, err
		}

		if source.Team != vol.Team {
			return nil, status.Error(codes.InvalidArgument, "source_volume must belong to the same team")
		}
	}

	revID, err := parseRequestID("source_revision_id", req.GetSourceRevisionId())
	if err != nil {
		return nil, err
	}

	incID, err := parseRequestID("incarnation_id", req.GetIncarnationId())
	if err != nil {
		return nil, err
	}

	opts, err := cloneOptions(req.GetSourceConsistency(), req.GetMode())
	if err != nil {
		return nil, err
	}

	info, err := s.backend.Clone(ctx, op,
		RevisionRef{VolumeRef: source, Revision: revID},
		IncarnationRef{VolumeRef: vol, Incarnation: incID},
		opts,
	)
	if err != nil {
		return nil, toStatus(err)
	}

	return &orchestrator.CloneBlockVolumeResponse{Incarnation: s.incarnationToProto(info)}, nil
}

// cloneOptions derives the clone behaviour from the source revision's
// recorded consistency and the intended use. The filesystem UUID is preserved
// exactly when a memory image is restored on the clone (the restored kernel
// has that filesystem mounted); every other combination is refused rather
// than guessed: a paired-memory image is only consistent together with its
// memory, and a quiesced image has no memory to restore.
func cloneOptions(consistency orchestrator.BlockVolumeConsistency, mode orchestrator.BlockVolumeCloneMode) (CloneOptions, error) {
	switch {
	case mode == orchestrator.BlockVolumeCloneMode_BLOCK_VOLUME_CLONE_MODE_COLD_BOOT &&
		consistency == orchestrator.BlockVolumeConsistency_BLOCK_VOLUME_CONSISTENCY_FILESYSTEM_QUIESCED:
		return CloneOptions{}, nil
	case mode == orchestrator.BlockVolumeCloneMode_BLOCK_VOLUME_CLONE_MODE_MEMORY_RESTORE &&
		consistency == orchestrator.BlockVolumeConsistency_BLOCK_VOLUME_CONSISTENCY_PAIRED_MEMORY:
		return CloneOptions{PreserveFilesystemUUID: true}, nil
	case mode == orchestrator.BlockVolumeCloneMode_BLOCK_VOLUME_CLONE_MODE_UNSPECIFIED:
		return CloneOptions{}, status.Error(codes.InvalidArgument, "clone mode is required")
	case consistency == orchestrator.BlockVolumeConsistency_BLOCK_VOLUME_CONSISTENCY_UNSPECIFIED:
		return CloneOptions{}, status.Error(codes.InvalidArgument, "source_consistency is required")
	default:
		return CloneOptions{}, status.Errorf(codes.InvalidArgument, "a %s revision cannot be cloned for %s", consistency, mode)
	}
}

func (s *GRPCServer) SealBlockVolume(ctx context.Context, req *orchestrator.SealBlockVolumeRequest) (*orchestrator.SealBlockVolumeResponse, error) {
	op, err := parseRequestID("operation_id", req.GetOperationId())
	if err != nil {
		return nil, err
	}

	vol, err := parseVolumeRef(req.GetVolume())
	if err != nil {
		return nil, err
	}

	incID, err := parseRequestID("incarnation_id", req.GetIncarnationId())
	if err != nil {
		return nil, err
	}

	revID, err := parseRequestID("revision_id", req.GetRevisionId())
	if err != nil {
		return nil, err
	}

	consistency, err := consistencyFromProto(req.GetConsistency())
	if err != nil {
		return nil, err
	}

	info, err := s.backend.Seal(ctx, op, IncarnationRef{VolumeRef: vol, Incarnation: incID}, revID, req.GetExecutionId())
	if err != nil {
		return nil, toStatus(err)
	}

	// The backend never decides consistency; the caller ran the protocol.
	info.Consistency = consistency

	return &orchestrator.SealBlockVolumeResponse{Revision: revisionToProto(info)}, nil
}

func (s *GRPCServer) DeleteBlockVolume(ctx context.Context, req *orchestrator.DeleteBlockVolumeRequest) (*orchestrator.DeleteBlockVolumeResponse, error) {
	op, err := parseRequestID("operation_id", req.GetOperationId())
	if err != nil {
		return nil, err
	}

	vol, err := parseVolumeRef(req.GetVolume())
	if err != nil {
		return nil, err
	}

	revs, err := parseRequestIDs("revision_ids", req.GetRevisionIds())
	if err != nil {
		return nil, err
	}

	incs, err := parseRequestIDs("incarnation_ids", req.GetIncarnationIds())
	if err != nil {
		return nil, err
	}

	// Revisions first: an incarnation's clones stay valid without it (Btrfs
	// shares extents, not parentage), but deleting revisions before their
	// source keeps a crash from leaving a revision whose incarnation is gone
	// and whose row says it is still there.
	for _, id := range revs {
		if err := s.backend.DeleteRevision(ctx, op, RevisionRef{VolumeRef: vol, Revision: id}); err != nil {
			return nil, toStatus(err)
		}
	}

	for _, id := range incs {
		if err := s.backend.DeleteIncarnation(ctx, op, IncarnationRef{VolumeRef: vol, Incarnation: id}); err != nil {
			return nil, toStatus(err)
		}
	}

	return &orchestrator.DeleteBlockVolumeResponse{}, nil
}

func (s *GRPCServer) ReconcileBlockVolume(ctx context.Context, req *orchestrator.ReconcileBlockVolumeRequest) (*orchestrator.ReconcileBlockVolumeResponse, error) {
	vol, err := parseVolumeRef(req.GetVolume())
	if err != nil {
		return nil, err
	}

	incIDs, err := parseRequestIDs("incarnation_ids", req.GetIncarnationIds())
	if err != nil {
		return nil, err
	}

	revIDs, err := parseRequestIDs("revision_ids", req.GetRevisionIds())
	if err != nil {
		return nil, err
	}

	heldIDs, err := parseRequestIDs("held_incarnation_ids", req.GetHeldIncarnationIds())
	if err != nil {
		return nil, err
	}

	// The backend compares the whole node; holders of other volumes are only
	// told apart by expecting them, so expected holders of this volume are
	// matched against its incarnations below.
	expected := Expected{Holders: heldIDs}
	for _, id := range incIDs {
		expected.Incarnations = append(expected.Incarnations, IncarnationRef{VolumeRef: vol, Incarnation: id})
	}

	for _, id := range revIDs {
		expected.Revisions = append(expected.Revisions, RevisionRef{VolumeRef: vol, Revision: id})
	}

	diff, err := s.backend.Reconcile(ctx, expected)
	if err != nil {
		return nil, toStatus(err)
	}

	return reconcileToProto(vol, incIDs, heldIDs, diff), nil
}

// reconcileToProto narrows a node-wide Diff to one volume. Holders are keyed
// by incarnation only, so a holder belongs to the volume when its
// incarnation is one the caller listed, is expected held, or is an
// unexpected incarnation of this volume on disk.
func reconcileToProto(vol VolumeRef, incIDs, heldIDs []ID, diff Diff) *orchestrator.ReconcileBlockVolumeResponse {
	resp := &orchestrator.ReconcileBlockVolumeResponse{}

	ours := map[ID]bool{}
	for _, id := range incIDs {
		ours[id] = true
	}

	for _, id := range heldIDs {
		ours[id] = true
	}

	for _, r := range diff.MissingIncarnations {
		if r.VolumeRef == vol {
			resp.MissingIncarnationIds = append(resp.MissingIncarnationIds, r.Incarnation.String())
		}
	}

	for _, r := range diff.UnexpectedIncarnations {
		if r.VolumeRef == vol {
			ours[r.Incarnation] = true
			resp.UnexpectedIncarnationIds = append(resp.UnexpectedIncarnationIds, r.Incarnation.String())
		}
	}

	for _, r := range diff.MissingRevisions {
		if r.VolumeRef == vol {
			resp.MissingRevisionIds = append(resp.MissingRevisionIds, r.Revision.String())
		}
	}

	for _, r := range diff.UnexpectedRevisions {
		if r.VolumeRef == vol {
			resp.UnexpectedRevisionIds = append(resp.UnexpectedRevisionIds, r.Revision.String())
		}
	}

	for _, id := range diff.MissingHolders {
		resp.MissingHolderIncarnationIds = append(resp.MissingHolderIncarnationIds, id.String())
	}

	for _, h := range diff.UnexpectedHolders {
		if ours[h.Incarnation] {
			resp.UnexpectedHolders = append(resp.UnexpectedHolders, holderToProto(h))
		}
	}

	for _, h := range diff.BlockedHolders {
		if ours[h.Incarnation] {
			resp.BlockedHolders = append(resp.BlockedHolders, holderToProto(h))
		}
	}

	for _, id := range diff.StagingOperations {
		resp.StagingOperationIds = append(resp.StagingOperationIds, id.String())
	}

	resp.Clean = len(resp.GetMissingIncarnationIds()) == 0 && len(resp.GetUnexpectedIncarnationIds()) == 0 &&
		len(resp.GetMissingRevisionIds()) == 0 && len(resp.GetUnexpectedRevisionIds()) == 0 &&
		len(resp.GetMissingHolderIncarnationIds()) == 0 && len(resp.GetUnexpectedHolders()) == 0 &&
		len(resp.GetBlockedHolders()) == 0

	return resp
}

func (s *GRPCServer) ForgetBlockVolumeOperation(ctx context.Context, req *orchestrator.ForgetBlockVolumeOperationRequest) (*orchestrator.ForgetBlockVolumeOperationResponse, error) {
	op, err := parseRequestID("operation_id", req.GetOperationId())
	if err != nil {
		return nil, err
	}

	if err := s.backend.ForgetOperation(ctx, op); err != nil {
		return nil, toStatus(err)
	}

	return &orchestrator.ForgetBlockVolumeOperationResponse{}, nil
}

func parseRequestID(field, value string) (ID, error) {
	id, err := ParseID(value)
	if err != nil {
		return ID{}, status.Errorf(codes.InvalidArgument, "%s: %v", field, err)
	}

	return id, nil
}

func parseRequestIDs(field string, values []string) ([]ID, error) {
	ids := make([]ID, 0, len(values))
	for i, v := range values {
		id, err := parseRequestID(fmt.Sprintf("%s[%d]", field, i), v)
		if err != nil {
			return nil, err
		}

		ids = append(ids, id)
	}

	return ids, nil
}

func parseVolumeRef(ref *orchestrator.BlockVolumeRef) (VolumeRef, error) {
	team, err := parseRequestID("volume.team_id", ref.GetTeamId())
	if err != nil {
		return VolumeRef{}, err
	}

	vol, err := parseRequestID("volume.volume_id", ref.GetVolumeId())
	if err != nil {
		return VolumeRef{}, err
	}

	return VolumeRef{Team: team, Volume: vol}, nil
}

func consistencyFromProto(c orchestrator.BlockVolumeConsistency) (Consistency, error) {
	switch c {
	case orchestrator.BlockVolumeConsistency_BLOCK_VOLUME_CONSISTENCY_FILESYSTEM_QUIESCED:
		return ConsistencyFilesystemQuiesced, nil
	case orchestrator.BlockVolumeConsistency_BLOCK_VOLUME_CONSISTENCY_PAIRED_MEMORY:
		return ConsistencyPairedMemory, nil
	default:
		return "", status.Errorf(codes.InvalidArgument, "unsupported consistency %s", c)
	}
}

// ConsistencyToProto maps a backend consistency to its wire value.
func ConsistencyToProto(c Consistency) orchestrator.BlockVolumeConsistency {
	switch c {
	case ConsistencyFilesystemQuiesced:
		return orchestrator.BlockVolumeConsistency_BLOCK_VOLUME_CONSISTENCY_FILESYSTEM_QUIESCED
	case ConsistencyPairedMemory:
		return orchestrator.BlockVolumeConsistency_BLOCK_VOLUME_CONSISTENCY_PAIRED_MEMORY
	default:
		return orchestrator.BlockVolumeConsistency_BLOCK_VOLUME_CONSISTENCY_UNSPECIFIED
	}
}

func (s *GRPCServer) incarnationToProto(info IncarnationInfo) *orchestrator.BlockVolumeIncarnation {
	subvol := filepath.Dir(info.ImagePath)
	if rel, err := filepath.Rel(s.root, subvol); err == nil {
		subvol = rel
	}

	out := &orchestrator.BlockVolumeIncarnation{
		IncarnationId:  info.Ref.Incarnation.String(),
		SubvolUuid:     info.SubvolumeUUID,
		FsUuid:         info.FilesystemUUID,
		SubvolPath:     subvol,
		CapacityBytes:  info.CapacityBytes,
		AllocatedBytes: info.AllocatedBytes,
	}
	if !info.ParentRevision.IsZero() {
		out.ParentRevisionId = info.ParentRevision.String()
	}

	return out
}

func revisionToProto(info RevisionInfo) *orchestrator.BlockVolumeRevision {
	out := &orchestrator.BlockVolumeRevision{
		RevisionId:          info.Ref.Revision.String(),
		SourceIncarnationId: info.SourceIncarnation.String(),
		SubvolUuid:          info.SubvolumeUUID,
		ParentSubvolUuid:    info.ParentSubvolumeUUID,
		FsUuid:              info.FilesystemUUID,
		SizeBytes:           info.CapacityBytes,
		AllocatedBytes:      info.AllocatedBytes,
		Consistency:         ConsistencyToProto(info.Consistency),
	}
	if !info.SourceOperation.IsZero() {
		out.SourceOperationId = info.SourceOperation.String()
	}

	if !info.SealedAt.IsZero() {
		out.SealedAt = timestamppb.New(info.SealedAt)
	}

	return out
}

func holderToProto(h Holder) *orchestrator.BlockVolumeHolder {
	return &orchestrator.BlockVolumeHolder{
		IncarnationId: h.Incarnation.String(),
		SandboxId:     h.SandboxID,
		ExecutionId:   h.ExecutionID,
		State:         string(h.State),
		Reason:        h.BlockedReason,
	}
}

// toStatus maps backend errors to gRPC codes. Tool stderr goes into an
// ErrorInfo detail, never into the status message, which callers log and may
// surface.
func toStatus(err error) error {
	if err == nil {
		return nil
	}

	if _, ok := status.FromError(err); ok {
		return err
	}

	var (
		cmdErr     *CommandError
		heldErr    *HeldByError
		blockedErr *BlockedError
		runningErr *StillRunningError
	)

	switch {
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	case errors.Is(err, ErrInvalidID), errors.Is(err, ErrReclaimEvidenceMismatch):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, ErrOperationConflict):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, ErrRevisionNotFound), errors.Is(err, ErrIncarnationNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, ErrResourceExhausted):
		return withInfo(codes.ResourceExhausted, err.Error(), "CAPACITY", nil)
	case errors.As(err, &blockedErr):
		return withInfo(codes.FailedPrecondition, err.Error(), "INCARNATION_BLOCKED", holderMetadata(blockedErr.Holder))
	case errors.As(err, &heldErr):
		return withInfo(codes.FailedPrecondition, err.Error(), "INCARNATION_HELD", holderMetadata(heldErr.Holder))
	case errors.As(err, &runningErr):
		return withInfo(codes.FailedPrecondition, err.Error(), "HOLDER_RUNNING", holderMetadata(runningErr.Holder))
	case errors.Is(err, ErrIncarnationHeld):
		return withInfo(codes.FailedPrecondition, err.Error(), "INCARNATION_HELD", nil)
	case errors.Is(err, ErrHolderMismatch):
		return status.Error(codes.PermissionDenied, err.Error())
	case errors.As(err, &cmdErr):
		return withInfo(codes.Internal, fmt.Sprintf("blockvol: %s failed with exit code %d", cmdErr.Name, cmdErr.ExitCode), "TOOL_FAILED", map[string]string{
			"tool":      cmdErr.Name,
			"exit_code": strconv.Itoa(cmdErr.ExitCode),
			"stderr":    cmdErr.Stderr,
		})
	default:
		return status.Error(codes.Internal, err.Error())
	}
}

func holderMetadata(h Holder) map[string]string {
	return map[string]string{
		"incarnation_id": h.Incarnation.String(),
		"sandbox_id":     h.SandboxID,
		"execution_id":   h.ExecutionID,
		"state":          string(h.State),
	}
}

func withInfo(code codes.Code, msg, reason string, metadata map[string]string) error {
	st := status.New(code, msg)

	detailed, err := st.WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: errorDomain, Metadata: metadata})
	if err != nil {
		return st.Err()
	}

	return detailed.Err()
}
