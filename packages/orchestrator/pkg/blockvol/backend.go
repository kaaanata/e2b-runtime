package blockvol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
)

// Consistency says what a revision's ext4 image is consistent with. The
// backend never decides this; the sandbox package sets it from the protocol
// it ran (fsfreeze vs. full-memory checkpoint).
type Consistency string

const (
	ConsistencyFilesystemQuiesced Consistency = "filesystem-quiesced"
	ConsistencyPairedMemory       Consistency = "paired-memory"
)

var (
	// ErrRevisionNotFound is returned when a clone or delete names a revision
	// that is not on disk.
	ErrRevisionNotFound = errors.New("blockvol: revision not found")
	// ErrIncarnationNotFound is returned when a seal names a missing incarnation.
	ErrIncarnationNotFound = errors.New("blockvol: incarnation not found")
	// ErrIncarnationHeld is returned when a delete finds a holder record; the
	// caller must Release (or reclaim) first.
	ErrIncarnationHeld = errors.New("blockvol: incarnation has a holder record")
	// ErrOperationConflict is returned when an operation id is replayed with a
	// different kind or different arguments.
	ErrOperationConflict = errors.New("blockvol: operation id reused with different parameters")
)

// IncarnationInfo describes a writable image on disk.
type IncarnationInfo struct {
	Ref            IncarnationRef `json:"ref"`
	ParentRevision ID             `json:"parent_revision,omitzero"`
	SubvolumeUUID  string         `json:"subvolume_uuid"`
	FilesystemUUID string         `json:"filesystem_uuid"`
	ImagePath      string         `json:"image_path"`
	CapacityBytes  uint64         `json:"capacity_bytes"`
	AllocatedBytes uint64         `json:"allocated_bytes"`
}

// RevisionInfo describes a sealed snapshot. Consistency is left empty by the
// backend for the caller to fill.
type RevisionInfo struct {
	Ref                 RevisionRef `json:"ref"`
	SourceIncarnation   ID          `json:"source_incarnation"`
	SourceOperation     ID          `json:"source_operation"`
	SubvolumeUUID       string      `json:"subvolume_uuid"`
	ParentSubvolumeUUID string      `json:"parent_subvolume_uuid"`
	FilesystemUUID      string      `json:"filesystem_uuid"`
	ImagePath           string      `json:"image_path"`
	CapacityBytes       uint64      `json:"capacity_bytes"`
	AllocatedBytes      uint64      `json:"allocated_bytes"`
	SealedAt            time.Time   `json:"sealed_at"`
	Consistency         Consistency `json:"consistency,omitempty"`
}

// CreateResult is the pair produced by Create: the writable incarnation and
// its revision 0.
type CreateResult struct {
	Incarnation IncarnationInfo `json:"incarnation"`
	Revision    RevisionInfo    `json:"revision"`
}

// Expected is the database's view handed to Reconcile.
type Expected struct {
	Incarnations []IncarnationRef
	Revisions    []RevisionRef
	// Holders lists incarnations that should have an active holder on this node.
	Holders []ID
}

// Diff is what Reconcile found. Nothing in it has been acted upon.
type Diff struct {
	MissingIncarnations    []IncarnationRef
	UnexpectedIncarnations []IncarnationRef
	MissingRevisions       []RevisionRef
	UnexpectedRevisions    []RevisionRef
	MissingHolders         []ID
	UnexpectedHolders      []Holder
	BlockedHolders         []Holder
	// StagingOperations are operation ids with leftovers under staging/;
	// whether they are garbage depends on the operation ledger.
	StagingOperations []ID
	// Invalid lists paths inside the tree that do not fit the layout.
	Invalid []string
}

// Clean reports whether physical state matches expectations exactly.
func (d Diff) Clean() bool {
	return len(d.MissingIncarnations) == 0 && len(d.UnexpectedIncarnations) == 0 &&
		len(d.MissingRevisions) == 0 && len(d.UnexpectedRevisions) == 0 &&
		len(d.MissingHolders) == 0 && len(d.UnexpectedHolders) == 0 &&
		len(d.BlockedHolders) == 0 && len(d.StagingOperations) == 0 && len(d.Invalid) == 0
}

// Backend is the orchestrator-internal contract for workspace block volumes.
// Every mutating method is idempotent by operation id: replaying an operation
// returns the recorded result and performs no further filesystem change.
type Backend interface {
	Create(ctx context.Context, op ID, inc IncarnationRef, revision ID, capacityBytes uint64) (CreateResult, error)
	Clone(ctx context.Context, op ID, from RevisionRef, inc IncarnationRef, opts CloneOptions) (IncarnationInfo, error)
	Seal(ctx context.Context, op ID, inc IncarnationRef, revision ID, executionID string) (RevisionInfo, error)
	DeleteRevision(ctx context.Context, op ID, rev RevisionRef) error
	DeleteIncarnation(ctx context.Context, op ID, inc IncarnationRef) error
	Bind(ctx context.Context, req BindRequest) (Holder, error)
	Release(ctx context.Context, req ReleaseRequest) error
	Reconcile(ctx context.Context, expected Expected) (Diff, error)
	Placeholder(ctx context.Context, capacityBytes uint64) (string, error)
	ForgetOperation(ctx context.Context, op ID) error
}

// CloneOptions tunes Clone for the kind of revision being forked.
type CloneOptions struct {
	// PreserveFilesystemUUID keeps the clone byte-identical to the revision.
	// It is required when the revision is ConsistencyPairedMemory and the
	// clone will be restored together with that memory image: the restored
	// guest kernel still has the filesystem mounted with the old UUID and
	// checksum seed cached in memory, so rewriting the superblock (or, worse,
	// replaying the journal with e2fsck) under it would corrupt the volume.
	// The two siblings run in different VMs, so sharing a UUID is harmless.
	// Cold-boot forks from ConsistencyFilesystemQuiesced revisions leave it
	// false and get a fresh UUID.
	PreserveFilesystemUUID bool `json:"preserve_filesystem_uuid,omitempty"`
}

// Config wires a Service. Zero values pick production defaults.
type Config struct {
	// Root overrides $BLOCK_VOLUME_ROOT / DefaultRoot.
	Root       string
	Watermarks Watermarks
	Runner     Runner
	Liveness   ProcessLiveness
	// BootID overrides the procfs boot id (tests only).
	BootID string
}

// Service implements Backend on a Btrfs directory tree.
type Service struct {
	layout     Layout
	tools      tools
	watermarks Watermarks
	gate       *Gate
}

var _ Backend = (*Service)(nil)

// New builds a Service and creates the fixed top-level directories.
func New(cfg Config) (*Service, error) {
	layout, err := NewLayout(cfg.Root)
	if err != nil {
		return nil, err
	}

	if err := layout.EnsureDirs(); err != nil {
		return nil, err
	}

	runner := cfg.Runner
	if runner == nil {
		runner = ExecRunner{}
	}

	liveness := cfg.Liveness
	if liveness == nil {
		liveness = ProcLiveness{}
	}

	wm := cfg.Watermarks
	if wm == (Watermarks{}) {
		wm = DefaultWatermarks
	}

	var gate *Gate
	if cfg.BootID != "" {
		gate = newGate(layout, liveness, cfg.BootID, time.Now)
	} else {
		gate, err = NewGate(layout, liveness)
		if err != nil {
			return nil, err
		}
	}

	return &Service{layout: layout, tools: tools{run: runner}, watermarks: wm, gate: gate}, nil
}

// Layout exposes the path mapping for the sandbox package.
func (s *Service) Layout() Layout { return s.layout }

// Gate exposes the admission gate for startup reclaim.
func (s *Service) Gate() *Gate { return s.gate }

// Bind admits a writer through the Gate after checking that the
// incarnation image exists, so a holder never points at a missing file.
func (s *Service) Bind(ctx context.Context, req BindRequest) (Holder, error) {
	if err := req.Incarnation.validate(); err != nil {
		return Holder{}, err
	}

	ok, err := exists(s.layout.IncarnationImage(req.Incarnation))
	if err != nil {
		return Holder{}, err
	}

	if !ok {
		return Holder{}, fmt.Errorf("%w: %s", ErrIncarnationNotFound, req.Incarnation.Incarnation)
	}

	return s.gate.Bind(ctx, req)
}

// Release delegates to the Gate.
func (s *Service) Release(ctx context.Context, req ReleaseRequest) error {
	return s.gate.Release(ctx, req)
}

// Create materialises a new incarnation with a fresh fixed-capacity ext4 and
// immediately seals it as revision 0. Steps run inside staging/<op>/ and are
// published with rename, so a crash leaves either nothing or a complete
// artefact in place; replaying the same op resumes from whatever was
// published and finally returns the recorded result.
func (s *Service) Create(ctx context.Context, op ID, inc IncarnationRef, revision ID, capacityBytes uint64) (CreateResult, error) {
	if err := inc.validate(); err != nil {
		return CreateResult{}, err
	}

	if op.IsZero() || revision.IsZero() {
		return CreateResult{}, fmt.Errorf("%w: create needs operation and revision ids", ErrInvalidID)
	}

	if capacityBytes == 0 {
		return CreateResult{}, errors.New("blockvol: create capacity must be positive")
	}

	type request struct {
		Inc      IncarnationRef `json:"inc"`
		Revision ID             `json:"revision"`
		Capacity uint64         `json:"capacity"`
	}

	req := request{Inc: inc, Revision: revision, Capacity: capacityBytes}

	var result CreateResult

	done, err := s.loadResult(op, "create", req, &result)
	if err != nil || done {
		return result, err
	}

	incDir := s.layout.IncarnationDir(inc)
	rev := RevisionRef{VolumeRef: inc.VolumeRef, Revision: revision}
	revDir := s.layout.RevisionDir(rev)

	incExists, err := exists(incDir)
	if err != nil {
		return CreateResult{}, err
	}

	if !incExists {
		if err := s.checkCapacity(ctx, capacityBytes); err != nil {
			return CreateResult{}, err
		}

		staged := filepath.Join(s.layout.StagingDir(op), incarnationsDir)
		if err := s.resetStaging(ctx, staged); err != nil {
			return CreateResult{}, err
		}

		if err := s.tools.subvolumeCreate(ctx, staged); err != nil {
			return CreateResult{}, err
		}

		image := filepath.Join(staged, ImageName)
		if err := s.tools.fallocate(ctx, image, capacityBytes); err != nil {
			return CreateResult{}, err
		}

		// A deterministic UUID keeps replays of the same operation identical.
		fsUUID := uuid.NewSHA1(op.UUID(), []byte(ImageName)).String()
		if err := s.tools.mkfsExt4(ctx, image, fsUUID); err != nil {
			return CreateResult{}, err
		}

		if err := fsyncPath(image); err != nil {
			return CreateResult{}, err
		}

		if err := fsyncPath(staged); err != nil {
			return CreateResult{}, err
		}

		if err := publish(staged, incDir); err != nil {
			return CreateResult{}, err
		}
	}

	if err := s.ensureRevision(ctx, incDir, revDir); err != nil {
		return CreateResult{}, err
	}

	incInfo, err := s.incarnationInfo(ctx, inc, ID{})
	if err != nil {
		return CreateResult{}, err
	}

	revInfo, err := s.revisionInfo(ctx, rev, inc.Incarnation, op)
	if err != nil {
		return CreateResult{}, err
	}

	result = CreateResult{Incarnation: incInfo, Revision: revInfo}

	if err := s.storeResult(op, "create", req, result); err != nil {
		return CreateResult{}, err
	}

	return result, nil
}

// Clone makes a new writable incarnation from a sealed revision. The ext4 gets
// a fresh UUID so a guest can mount the clone next to its ancestor.
//
// With opts.PreserveFilesystemUUID the image is left byte-identical; see
// CloneOptions for when that is required.
func (s *Service) Clone(ctx context.Context, op ID, from RevisionRef, inc IncarnationRef, opts CloneOptions) (IncarnationInfo, error) {
	if err := from.validate(); err != nil {
		return IncarnationInfo{}, err
	}

	if err := inc.validate(); err != nil {
		return IncarnationInfo{}, err
	}

	if op.IsZero() {
		return IncarnationInfo{}, fmt.Errorf("%w: clone needs an operation id", ErrInvalidID)
	}

	type request struct {
		From RevisionRef    `json:"from"`
		Inc  IncarnationRef `json:"inc"`
		Opts CloneOptions   `json:"opts"`
	}

	req := request{From: from, Inc: inc, Opts: opts}

	var result IncarnationInfo

	done, err := s.loadResult(op, "clone", req, &result)
	if err != nil || done {
		return result, err
	}

	incDir := s.layout.IncarnationDir(inc)

	incExists, err := exists(incDir)
	if err != nil {
		return IncarnationInfo{}, err
	}

	if !incExists {
		revDir := s.layout.RevisionDir(from)

		revExists, err := exists(revDir)
		if err != nil {
			return IncarnationInfo{}, err
		}

		if !revExists {
			return IncarnationInfo{}, fmt.Errorf("%w: %s", ErrRevisionNotFound, from.Revision)
		}

		if err := s.checkCapacity(ctx, 0); err != nil {
			return IncarnationInfo{}, err
		}

		staged := filepath.Join(s.layout.StagingDir(op), incarnationsDir)
		if err := s.resetStaging(ctx, staged); err != nil {
			return IncarnationInfo{}, err
		}

		if err := s.tools.subvolumeSnapshot(ctx, revDir, staged, false); err != nil {
			return IncarnationInfo{}, err
		}

		if !opts.PreserveFilesystemUUID {
			image := filepath.Join(staged, ImageName)
			if err := s.tools.tune2fsRandomUUID(ctx, image); err != nil {
				return IncarnationInfo{}, err
			}

			if err := fsyncPath(image); err != nil {
				return IncarnationInfo{}, err
			}
		}

		if err := fsyncPath(staged); err != nil {
			return IncarnationInfo{}, err
		}

		if err := publish(staged, incDir); err != nil {
			return IncarnationInfo{}, err
		}
	}

	result, err = s.incarnationInfo(ctx, inc, from.Revision)
	if err != nil {
		return IncarnationInfo{}, err
	}

	if err := s.storeResult(op, "clone", req, result); err != nil {
		return IncarnationInfo{}, err
	}

	return result, nil
}

// Seal takes a read-only snapshot of an incarnation. The caller guarantees the
// VM is paused and the fork has drained and fsynced the drive.
//
// executionID names the writer the caller believes owns the image: it must
// match the holder record. An empty executionID asserts that nobody is bound
// (offline seal of a released incarnation) and is refused while any holder
// record exists. Consistency is left for the caller to set.
func (s *Service) Seal(ctx context.Context, op ID, inc IncarnationRef, revision ID, executionID string) (RevisionInfo, error) {
	if err := inc.validate(); err != nil {
		return RevisionInfo{}, err
	}

	if op.IsZero() || revision.IsZero() {
		return RevisionInfo{}, fmt.Errorf("%w: seal needs operation and revision ids", ErrInvalidID)
	}

	type request struct {
		Inc      IncarnationRef `json:"inc"`
		Revision ID             `json:"revision"`
	}

	req := request{Inc: inc, Revision: revision}

	var result RevisionInfo

	done, err := s.loadResult(op, "seal", req, &result)
	if err != nil || done {
		return result, err
	}

	holder, err := s.gate.Inspect(inc.Incarnation)
	if err != nil {
		return RevisionInfo{}, err
	}

	switch {
	case executionID == "" && holder != nil:
		return RevisionInfo{}, fmt.Errorf("%w: offline seal, but record held by %s (%s)", ErrHolderMismatch, holder.ExecutionID, holder.State)
	case executionID != "" && holder == nil:
		return RevisionInfo{}, fmt.Errorf("%w: seal by %s, but incarnation has no holder", ErrHolderMismatch, executionID)
	case executionID != "" && holder.ExecutionID != executionID:
		return RevisionInfo{}, fmt.Errorf("%w: seal by %s, record held by %s", ErrHolderMismatch, executionID, holder.ExecutionID)
	}

	incDir := s.layout.IncarnationDir(inc)
	rev := RevisionRef{VolumeRef: inc.VolumeRef, Revision: revision}
	revDir := s.layout.RevisionDir(rev)

	revExists, err := exists(revDir)
	if err != nil {
		return RevisionInfo{}, err
	}

	if !revExists {
		incExists, err := exists(incDir)
		if err != nil {
			return RevisionInfo{}, err
		}

		if !incExists {
			return RevisionInfo{}, fmt.Errorf("%w: %s", ErrIncarnationNotFound, inc.Incarnation)
		}

		if err := s.checkCapacity(ctx, 0); err != nil {
			return RevisionInfo{}, err
		}

		if err := s.ensureRevision(ctx, incDir, revDir); err != nil {
			return RevisionInfo{}, err
		}
	}

	result, err = s.revisionInfo(ctx, rev, inc.Incarnation, op)
	if err != nil {
		return RevisionInfo{}, err
	}

	if err := s.storeResult(op, "seal", req, result); err != nil {
		return RevisionInfo{}, err
	}

	return result, nil
}

// DeleteRevision removes a sealed snapshot. Reference checks (snapshot refs,
// child incarnations) are the caller's job; Btrfs itself keeps clones valid.
func (s *Service) DeleteRevision(ctx context.Context, op ID, rev RevisionRef) error {
	if err := rev.validate(); err != nil {
		return err
	}

	if op.IsZero() {
		return fmt.Errorf("%w: delete needs an operation id", ErrInvalidID)
	}

	if err := s.tools.subvolumeDelete(ctx, s.layout.RevisionDir(rev)); err != nil {
		return err
	}

	return s.pruneVolumeDirs(rev.VolumeRef)
}

// DeleteIncarnation removes a writable image. It refuses while any holder
// record exists; the caller must Release or reclaim first.
func (s *Service) DeleteIncarnation(ctx context.Context, op ID, inc IncarnationRef) error {
	if err := inc.validate(); err != nil {
		return err
	}

	if op.IsZero() {
		return fmt.Errorf("%w: delete needs an operation id", ErrInvalidID)
	}

	holder, err := s.gate.Inspect(inc.Incarnation)
	if err != nil {
		return err
	}

	if holder != nil {
		return fmt.Errorf("%w: incarnation %s, state %s, execution %s", ErrIncarnationHeld, inc.Incarnation, holder.State, holder.ExecutionID)
	}

	if err := s.tools.subvolumeDelete(ctx, s.layout.IncarnationDir(inc)); err != nil {
		return err
	}

	return s.pruneVolumeDirs(inc.VolumeRef)
}

// ForgetOperation drops staging/<op>/ including its result record and any
// half-built subvolume. The operation ledger calls it once the operation's
// outcome is committed in the database; after that a replay of the same id
// would redo the work, so it must never be called for an operation that may
// still be retried. Staging is cleaned by operation state, never by mtime.
func (s *Service) ForgetOperation(ctx context.Context, op ID) error {
	if op.IsZero() {
		return fmt.Errorf("%w: forget needs an operation id", ErrInvalidID)
	}

	dir := s.layout.StagingDir(op)

	// Only incarnations are staged (see ensureRevision); a failed Create or
	// Clone may have left that writable subvolume behind.
	if err := s.tools.subvolumeDelete(ctx, filepath.Join(dir, incarnationsDir)); err != nil {
		return err
	}

	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("blockvol: remove staging %s: %w", dir, err)
	}

	return fsyncPath(s.layout.StagingRoot())
}

// Reconcile compares the physical tree and holder records with what the
// database expects. It only reads.
func (s *Service) Reconcile(ctx context.Context, expected Expected) (Diff, error) {
	if err := ctx.Err(); err != nil {
		return Diff{}, err
	}

	var diff Diff

	physIncs, physRevs, invalid, err := s.walkTeams()
	if err != nil {
		return Diff{}, err
	}

	diff.Invalid = invalid

	wantIncs := make(map[IncarnationRef]bool, len(expected.Incarnations))
	for _, r := range expected.Incarnations {
		wantIncs[r] = true
	}

	wantRevs := make(map[RevisionRef]bool, len(expected.Revisions))
	for _, r := range expected.Revisions {
		wantRevs[r] = true
	}

	for ref := range physIncs {
		if !wantIncs[ref] {
			diff.UnexpectedIncarnations = append(diff.UnexpectedIncarnations, ref)
		}
	}

	for ref := range wantIncs {
		if !physIncs[ref] {
			diff.MissingIncarnations = append(diff.MissingIncarnations, ref)
		}
	}

	for ref := range physRevs {
		if !wantRevs[ref] {
			diff.UnexpectedRevisions = append(diff.UnexpectedRevisions, ref)
		}
	}

	for ref := range wantRevs {
		if !physRevs[ref] {
			diff.MissingRevisions = append(diff.MissingRevisions, ref)
		}
	}

	holders, err := s.gate.List()
	if err != nil {
		return Diff{}, err
	}

	wantHolders := make(map[ID]bool, len(expected.Holders))
	for _, id := range expected.Holders {
		wantHolders[id] = true
	}

	seenHolders := make(map[ID]bool, len(holders))
	for _, h := range holders {
		seenHolders[h.Incarnation] = true

		if h.State == HolderBlocked {
			diff.BlockedHolders = append(diff.BlockedHolders, h)
		}

		if !wantHolders[h.Incarnation] {
			diff.UnexpectedHolders = append(diff.UnexpectedHolders, h)
		}
	}

	for id := range wantHolders {
		if !seenHolders[id] {
			diff.MissingHolders = append(diff.MissingHolders, id)
		}
	}

	staging, err := os.ReadDir(s.layout.StagingRoot())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Diff{}, fmt.Errorf("blockvol: list staging: %w", err)
	}

	for _, e := range staging {
		id, err := ParseID(e.Name())
		if err != nil {
			diff.Invalid = append(diff.Invalid, filepath.Join(s.layout.StagingRoot(), e.Name()))

			continue
		}

		diff.StagingOperations = append(diff.StagingOperations, id)
	}

	sortDiff(&diff)

	return diff, nil
}

// walkTeams enumerates teams/<team>/<volume>/{inc,rev}/<id>.
func (s *Service) walkTeams() (map[IncarnationRef]bool, map[RevisionRef]bool, []string, error) {
	incs := map[IncarnationRef]bool{}
	revs := map[RevisionRef]bool{}

	var invalid []string

	teams, err := os.ReadDir(s.layout.TeamsDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return incs, revs, nil, nil
		}

		return nil, nil, nil, fmt.Errorf("blockvol: list teams: %w", err)
	}

	for _, teamEntry := range teams {
		teamID, err := ParseID(teamEntry.Name())
		if err != nil || !teamEntry.IsDir() {
			invalid = append(invalid, filepath.Join(s.layout.TeamsDir(), teamEntry.Name()))

			continue
		}

		volumes, err := os.ReadDir(s.layout.TeamDir(teamID))
		if err != nil {
			return nil, nil, nil, fmt.Errorf("blockvol: list team %s: %w", teamID, err)
		}

		for _, volEntry := range volumes {
			volID, err := ParseID(volEntry.Name())
			if err != nil || !volEntry.IsDir() {
				invalid = append(invalid, filepath.Join(s.layout.TeamDir(teamID), volEntry.Name()))

				continue
			}

			vol := VolumeRef{Team: teamID, Volume: volID}

			volInvalid, err := s.walkVolume(vol, incs, revs)
			if err != nil {
				return nil, nil, nil, err
			}

			invalid = append(invalid, volInvalid...)
		}
	}

	return incs, revs, invalid, nil
}

func (s *Service) walkVolume(vol VolumeRef, incs map[IncarnationRef]bool, revs map[RevisionRef]bool) ([]string, error) {
	var invalid []string

	children, err := os.ReadDir(s.layout.VolumeDir(vol))
	if err != nil {
		return nil, fmt.Errorf("blockvol: list volume %s: %w", vol.Volume, err)
	}

	for _, child := range children {
		dir := filepath.Join(s.layout.VolumeDir(vol), child.Name())

		if child.Name() != incarnationsDir && child.Name() != revisionsDir {
			invalid = append(invalid, dir)

			continue
		}

		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("blockvol: list %s: %w", dir, err)
		}

		for _, e := range entries {
			id, err := ParseID(e.Name())
			if err != nil || !e.IsDir() {
				invalid = append(invalid, filepath.Join(dir, e.Name()))

				continue
			}

			if child.Name() == incarnationsDir {
				incs[IncarnationRef{VolumeRef: vol, Incarnation: id}] = true
			} else {
				revs[RevisionRef{VolumeRef: vol, Revision: id}] = true
			}
		}
	}

	return invalid, nil
}

func sortDiff(d *Diff) {
	byInc := func(a, b IncarnationRef) int { return strings.Compare(a.Incarnation.s, b.Incarnation.s) }
	byRev := func(a, b RevisionRef) int { return strings.Compare(a.Revision.s, b.Revision.s) }
	byHolder := func(a, b Holder) int { return strings.Compare(a.Incarnation.s, b.Incarnation.s) }
	byID := func(a, b ID) int { return strings.Compare(a.s, b.s) }

	slices.SortFunc(d.MissingIncarnations, byInc)
	slices.SortFunc(d.UnexpectedIncarnations, byInc)
	slices.SortFunc(d.MissingRevisions, byRev)
	slices.SortFunc(d.UnexpectedRevisions, byRev)
	slices.SortFunc(d.MissingHolders, byID)
	slices.SortFunc(d.UnexpectedHolders, byHolder)
	slices.SortFunc(d.BlockedHolders, byHolder)
	slices.SortFunc(d.StagingOperations, byID)
	slices.Sort(d.Invalid)
}

// ensureRevision snapshots incDir read-only into revDir unless revDir
// already exists.
//
// Unlike incarnations, revisions are not built in staging/ and renamed into
// place: Btrfs refuses to move a read-only subvolume to another parent
// directory (rename fails with EROFS because the subvolume's ".." entry would
// change; observed on 7.0.14-pve with btrfs-progs 6.14). Staging a writable
// snapshot and flipping it read-only after publish would expose a writable
// published revision across a crash. Instead the read-only snapshot is
// created directly at its final path: snapshot creation is a single Btrfs
// transaction that the ioctl commits before returning, so after a crash the
// revision either exists completely or not at all, and a replay that finds it
// reuses it.
func (s *Service) ensureRevision(ctx context.Context, incDir, revDir string) error {
	revExists, err := exists(revDir)
	if err != nil {
		return err
	}

	if revExists {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(revDir), 0o700); err != nil {
		return fmt.Errorf("blockvol: create parent of %s: %w", revDir, err)
	}

	if err := s.tools.subvolumeSnapshot(ctx, incDir, revDir, true); err != nil {
		return err
	}

	return fsyncPath(filepath.Dir(revDir))
}

// resetStaging makes sure staged is absent (deleting a half-built subvolume
// from an earlier attempt) and that its parent exists.
func (s *Service) resetStaging(ctx context.Context, staged string) error {
	if err := os.MkdirAll(filepath.Dir(staged), 0o700); err != nil {
		return fmt.Errorf("blockvol: create staging dir: %w", err)
	}

	return s.tools.subvolumeDelete(ctx, staged)
}

func (s *Service) checkCapacity(ctx context.Context, requiredData uint64) error {
	out, err := s.tools.filesystemUsage(ctx, s.layout.Root())
	if err != nil {
		return err
	}

	usage, err := ParseUsage(out)
	if err != nil {
		return err
	}

	return s.watermarks.Check(usage, requiredData)
}

func (s *Service) incarnationInfo(ctx context.Context, inc IncarnationRef, parent ID) (IncarnationInfo, error) {
	dir := s.layout.IncarnationDir(inc)
	image := s.layout.IncarnationImage(inc)

	sub, err := s.tools.subvolumeShow(ctx, dir)
	if err != nil {
		return IncarnationInfo{}, err
	}

	fsUUID, err := s.tools.ext4UUID(ctx, image)
	if err != nil {
		return IncarnationInfo{}, err
	}

	size, allocated, err := imageSize(image)
	if err != nil {
		return IncarnationInfo{}, err
	}

	return IncarnationInfo{
		Ref:            inc,
		ParentRevision: parent,
		SubvolumeUUID:  sub.UUID,
		FilesystemUUID: fsUUID,
		ImagePath:      image,
		CapacityBytes:  size,
		AllocatedBytes: allocated,
	}, nil
}

func (s *Service) revisionInfo(ctx context.Context, rev RevisionRef, source ID, op ID) (RevisionInfo, error) {
	dir := s.layout.RevisionDir(rev)
	image := s.layout.RevisionImage(rev)

	sub, err := s.tools.subvolumeShow(ctx, dir)
	if err != nil {
		return RevisionInfo{}, err
	}

	if !sub.ReadOnly {
		return RevisionInfo{}, fmt.Errorf("blockvol: revision %s subvolume is not read-only", rev.Revision)
	}

	fsUUID, err := s.tools.ext4UUID(ctx, image)
	if err != nil {
		return RevisionInfo{}, err
	}

	size, allocated, err := imageSize(image)
	if err != nil {
		return RevisionInfo{}, err
	}

	st, err := os.Stat(dir)
	if err != nil {
		return RevisionInfo{}, fmt.Errorf("blockvol: stat %s: %w", dir, err)
	}

	return RevisionInfo{
		Ref:                 rev,
		SourceIncarnation:   source,
		SourceOperation:     op,
		SubvolumeUUID:       sub.UUID,
		ParentSubvolumeUUID: sub.ParentUUID,
		FilesystemUUID:      fsUUID,
		ImagePath:           image,
		CapacityBytes:       size,
		AllocatedBytes:      allocated,
		SealedAt:            st.ModTime().UTC(),
	}, nil
}

// pruneVolumeDirs removes inc/, rev/ and the volume directory when they are
// empty, so a fully deleted volume leaves no trace. Non-empty is not an error.
func (s *Service) pruneVolumeDirs(vol VolumeRef) error {
	for _, dir := range []string{s.layout.IncarnationsDir(vol), s.layout.RevisionsDir(vol), s.layout.VolumeDir(vol), s.layout.TeamDir(vol.Team)} {
		err := os.Remove(dir)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			continue
		}

		if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
			return nil
		}

		return fmt.Errorf("blockvol: prune %s: %w", dir, err)
	}

	return nil
}

// operationRecord is the durable result of one operation under
// staging/<op>/result.json. Replays compare the request and return the result.
type operationRecord struct {
	Kind    string          `json:"kind"`
	Request json.RawMessage `json:"request"`
	Result  json.RawMessage `json:"result"`
}

func (s *Service) resultPath(op ID) string {
	return filepath.Join(s.layout.StagingDir(op), "result.json")
}

// loadResult returns true and fills out when the operation already completed
// with the same kind and request.
func (s *Service) loadResult(op ID, kind string, req any, out any) (bool, error) {
	raw, err := os.ReadFile(s.resultPath(op))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}

		return false, fmt.Errorf("blockvol: read operation result: %w", err)
	}

	var rec operationRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return false, fmt.Errorf("blockvol: corrupt operation result for %s: %w", op, err)
	}

	wantReq, err := json.Marshal(req)
	if err != nil {
		return false, fmt.Errorf("blockvol: encode request: %w", err)
	}

	if rec.Kind != kind || !bytes.Equal(bytes.TrimSpace(rec.Request), wantReq) {
		return false, fmt.Errorf("%w: operation %s recorded as %s", ErrOperationConflict, op, rec.Kind)
	}

	if err := json.Unmarshal(rec.Result, out); err != nil {
		return false, fmt.Errorf("blockvol: decode operation result for %s: %w", op, err)
	}

	return true, nil
}

func (s *Service) storeResult(op ID, kind string, req any, result any) error {
	reqRaw, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("blockvol: encode request: %w", err)
	}

	resRaw, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("blockvol: encode result: %w", err)
	}

	raw, err := json.Marshal(operationRecord{Kind: kind, Request: reqRaw, Result: resRaw})
	if err != nil {
		return fmt.Errorf("blockvol: encode operation record: %w", err)
	}

	path := s.resultPath(op)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("blockvol: create staging dir: %w", err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("blockvol: write %s: %w", tmp, err)
	}

	if err := fsyncPath(tmp); err != nil {
		return err
	}

	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("blockvol: publish operation result: %w", err)
	}

	return fsyncPath(filepath.Dir(path))
}
