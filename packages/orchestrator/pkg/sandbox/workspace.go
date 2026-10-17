//go:build linux

package sandbox

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/blockvol"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/envd"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/fc"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/metadata"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

// Errors of the workspace block volume lifecycle. Each is a sentinel matched
// with errors.Is; the server maps all three to codes.FailedPrecondition (the
// API answers 409): the request is well-formed but this sandbox, template or
// node cannot honour it, and nothing was changed.
var (
	// ErrCapabilityUnsupported refuses a lifecycle path that the first block
	// volume release does not support for a sandbox with bound workspace
	// volumes: a resume-fresh checkpoint, a deferred workspace seal, a
	// filesystem-only pause through an envd without the mount-set freeze, or a
	// node without a workspace backend. It is raised inside Pause and the resume
	// paths themselves, so every entry point (API pause, autopause, checkpoint,
	// prefetch harvest, envd upgrade) is covered, not just the handlers.
	ErrCapabilityUnsupported = errors.New("capability unsupported for sandboxes with workspace block volumes")

	// ErrWorkspaceLayout refuses workspace volumes the template cannot host: a
	// template that is not LayoutBlockV1, a slot index outside its reserved
	// slots, two volumes on one slot or one mount path.
	ErrWorkspaceLayout = errors.New("template layout cannot host the requested workspace volumes")

	// ErrWorkspaceRestore refuses a restore whose volumes do not match the
	// snapshot: a slot the snapshot recorded a revision for has no volume, a
	// paired-memory volume carries a different ext4 UUID than the revision, or
	// the capacity differs without a resize request (or the resize would shrink).
	ErrWorkspaceRestore = errors.New("workspace volume does not match the snapshot")
)

// WorkspaceFormatVersion is the on-disk format envd is told a block volume
// has: blockvol's fixed mkfs.ext4 options (4 KiB blocks, metadata_csum_seed).
const WorkspaceFormatVersion = 1

// workspaceReleaseWait bounds how long a sandbox teardown waits for the
// Firecracker process to exit before releasing its volumes. Past it the holder
// records stay in place (the gate later reports them blocked) rather than
// letting a still-running VM's volume be bound by someone else.
const workspaceReleaseWait = 30 * time.Second

// WorkspaceVolume binds one workspace slot of a sandbox to one incarnation (a
// writable image) of a workspace block volume for one execution. The control
// plane resolves which incarnation a slot gets (a fresh clone on every restore
// or fork); the orchestrator only admits the VM to it.
type WorkspaceVolume struct {
	// Slot is the index of the template's reserved workspace drive.
	Slot int
	// VolumeID and IncarnationID name the incarnation under the sandbox's team.
	VolumeID      string
	IncarnationID string
	// MountPath is where envd mounts the volume inside the guest.
	MountPath string
	// ExpectedFsUUID and ExpectedCapacityBytes, when set, are the control
	// plane's record of the incarnation; a mismatch with what is on disk fails
	// the start instead of mounting a volume the database does not describe.
	ExpectedFsUUID        string
	ExpectedCapacityBytes uint64
	// ResizeToBytes, when non-zero, grows the volume to this size before the
	// guest runs. Never smaller than the current size. A memory restore grows
	// the device and the guest grows the mounted filesystem online; a cold boot
	// grows the filesystem on the host before boot.
	ResizeToBytes uint64
}

// WorkspaceBackend is the part of blockvol the sandbox lifecycle uses.
type WorkspaceBackend interface {
	Bind(ctx context.Context, req blockvol.BindRequest) (blockvol.Holder, error)
	Release(ctx context.Context, req blockvol.ReleaseRequest) error
	Seal(ctx context.Context, op blockvol.ID, inc blockvol.IncarnationRef, revision blockvol.ID, executionID string) (blockvol.RevisionInfo, error)
	DescribeIncarnation(ctx context.Context, inc blockvol.IncarnationRef) (blockvol.IncarnationInfo, error)
	GrowImage(ctx context.Context, inc blockvol.IncarnationRef, executionID string, sizeBytes uint64) error
	GrowFilesystemOffline(ctx context.Context, inc blockvol.IncarnationRef, sizeBytes uint64) error
	Placeholder(ctx context.Context, capacityBytes uint64) (string, error)
}

var _ WorkspaceBackend = (*blockvol.Service)(nil)

// SealedWorkspace is one workspace revision a pause produced, with the
// identities the control plane needs to record it (snapshot_volume_refs).
type SealedWorkspace struct {
	Slot          int
	VolumeID      string
	IncarnationID string
	// OperationID is the blockvol operation that sealed the revision; replaying
	// the same pause (same build id) replays it.
	OperationID string
	Revision    metadata.WorkspaceRevisionRef
}

// workspaceMode is how the VM will see the volumes.
type workspaceMode int

const (
	// workspaceMemoryRestore loads a memory snapshot: the guest may already
	// have the volumes mounted, so identity and size must match the snapshot.
	workspaceMemoryRestore workspaceMode = iota
	// workspaceColdBoot boots a fresh kernel: the guest mounts from scratch.
	workspaceColdBoot
)

type boundWorkspace struct {
	WorkspaceVolume

	ref    blockvol.IncarnationRef
	image  string
	fsUUID string
	// sizeBytes is the image size the VM will see (after any grow).
	sizeBytes uint64
	// imageBytes is the image size found on disk before any grow.
	imageBytes uint64
	// revision is the snapshot's revision of this slot; nil when the snapshot
	// (e.g. a template base) never had a volume there.
	revision *metadata.WorkspaceRevisionRef
	// onlineResize: after envd is up, grow the mounted filesystem.
	onlineResize bool
	holder       *blockvol.Holder
}

// workspaceSet is a sandbox's workspace slots for one Firecracker process:
// every reserved slot gets a drive (its bound incarnation, or a placeholder of
// the recorded size), and the bound ones get a holder record.
type workspaceSet struct {
	backend     WorkspaceBackend
	sandboxID   string
	executionID string
	mode        workspaceMode
	layout      metadata.WorkspaceLayout
	bound       []*boundWorkspace
	drives      []fc.WorkspaceDrive
	// releaseWait overrides workspaceReleaseWait (tests).
	releaseWait time.Duration

	mu sync.Mutex
}

// planWorkspace resolves volumes against the template's layout and the
// snapshot's revisions, reading every bound incarnation from disk. It changes
// nothing; admit binds, prepareColdBoot grows offline. A nil set (and nil
// error) means the template has no workspace slots and no volume was asked for.
//
// executionID is the holder identity: it must be unique per Firecracker
// process, which is why callers pass the lifecycle id.
func planWorkspace(
	ctx context.Context,
	backend WorkspaceBackend,
	meta metadata.Template,
	volumes []WorkspaceVolume,
	teamID, sandboxID, executionID string,
	mode workspaceMode,
) (*workspaceSet, error) {
	if len(volumes) == 0 && !meta.HasWorkspaceSlots() {
		return nil, nil //nolint:nilnil // no workspace slots is the common case, not an error
	}

	if !meta.HasWorkspaceSlots() {
		return nil, fmt.Errorf("%w: template layout %q reserves no workspace slots, %d volume(s) requested", ErrWorkspaceLayout, meta.Layout, len(volumes))
	}

	if backend == nil {
		return nil, fmt.Errorf("%w: this node has no workspace block volume backend (BLOCK_VOLUME_ENABLED is off)", ErrCapabilityUnsupported)
	}

	layout := *meta.Workspace
	if len(layout.SlotCapacityBytes) != layout.Slots {
		return nil, fmt.Errorf("%w: template records %d slot capacities for %d slots", ErrWorkspaceLayout, len(layout.SlotCapacityBytes), layout.Slots)
	}

	if layout.Slots > fc.MaxWorkspaceSlots {
		return nil, fmt.Errorf("%w: template reserves %d slots, this orchestrator supports %d", ErrWorkspaceLayout, layout.Slots, fc.MaxWorkspaceSlots)
	}

	team, err := blockvol.ParseID(teamID)
	if err != nil {
		return nil, fmt.Errorf("%w: team id: %w", ErrWorkspaceLayout, err)
	}

	revisions := make(map[int]*metadata.WorkspaceRevisionRef, len(meta.WorkspaceRevisions))
	for i := range meta.WorkspaceRevisions {
		rev := &meta.WorkspaceRevisions[i]
		if rev.Slot < 0 || rev.Slot >= layout.Slots {
			return nil, fmt.Errorf("%w: snapshot records a revision for slot %d outside %d slots", ErrWorkspaceRestore, rev.Slot, layout.Slots)
		}

		revisions[rev.Slot] = rev
	}

	set := &workspaceSet{
		backend:     backend,
		sandboxID:   sandboxID,
		executionID: executionID,
		mode:        mode,
		layout:      layout,
	}

	bySlot := make(map[int]*boundWorkspace, len(volumes))
	paths := make(map[string]int, len(volumes))

	for _, v := range volumes {
		if v.Slot < 0 || v.Slot >= layout.Slots {
			return nil, fmt.Errorf("%w: slot %d outside the template's %d slots", ErrWorkspaceLayout, v.Slot, layout.Slots)
		}

		if _, dup := bySlot[v.Slot]; dup {
			return nil, fmt.Errorf("%w: slot %d bound twice", ErrWorkspaceLayout, v.Slot)
		}

		if v.MountPath == "" || !path.IsAbs(v.MountPath) || path.Clean(v.MountPath) != v.MountPath || v.MountPath == "/" {
			return nil, fmt.Errorf("%w: slot %d mount path %q must be a clean absolute path other than /", ErrWorkspaceLayout, v.Slot, v.MountPath)
		}

		if other, dup := paths[v.MountPath]; dup {
			return nil, fmt.Errorf("%w: slots %d and %d both mount at %s", ErrWorkspaceLayout, other, v.Slot, v.MountPath)
		}
		paths[v.MountPath] = v.Slot

		volID, err := blockvol.ParseID(v.VolumeID)
		if err != nil {
			return nil, fmt.Errorf("%w: slot %d volume id: %w", ErrWorkspaceLayout, v.Slot, err)
		}

		incID, err := blockvol.ParseID(v.IncarnationID)
		if err != nil {
			return nil, fmt.Errorf("%w: slot %d incarnation id: %w", ErrWorkspaceLayout, v.Slot, err)
		}

		ref := blockvol.IncarnationRef{VolumeRef: blockvol.VolumeRef{Team: team, Volume: volID}, Incarnation: incID}

		info, err := backend.DescribeIncarnation(ctx, ref)
		if err != nil {
			if errors.Is(err, blockvol.ErrIncarnationNotFound) {
				return nil, fmt.Errorf("%w: slot %d: %w", ErrWorkspaceRestore, v.Slot, err)
			}

			return nil, fmt.Errorf("describe workspace slot %d: %w", v.Slot, err)
		}

		b := &boundWorkspace{
			WorkspaceVolume: v,
			ref:             ref,
			image:           info.ImagePath,
			fsUUID:          info.FilesystemUUID,
			imageBytes:      info.CapacityBytes,
			sizeBytes:       info.CapacityBytes,
			revision:        revisions[v.Slot],
		}

		if err := b.check(mode, uint64(layout.SlotCapacityBytes[v.Slot])); err != nil { //nolint:gosec // capacities are positive tier sizes
			return nil, err
		}

		bySlot[v.Slot] = b
	}

	if mode == workspaceMemoryRestore {
		for slot := range revisions {
			if _, ok := bySlot[slot]; !ok {
				return nil, fmt.Errorf("%w: the snapshot has a volume mounted in slot %d but no incarnation was bound to it", ErrWorkspaceRestore, slot)
			}
		}
	}

	for slot := range layout.Slots {
		drive := fc.WorkspaceDrive{DriveID: fc.WorkspaceDriveID(slot)}

		if b, ok := bySlot[slot]; ok {
			drive.PathOnHost = b.image
			drive.RefreshSize = mode == workspaceMemoryRestore && b.sizeBytes != uint64(layout.SlotCapacityBytes[slot]) //nolint:gosec // positive tier size
			set.bound = append(set.bound, b)
		} else {
			placeholder, err := backend.Placeholder(ctx, uint64(layout.SlotCapacityBytes[slot])) //nolint:gosec // positive tier size
			if err != nil {
				return nil, fmt.Errorf("workspace slot %d placeholder: %w", slot, err)
			}

			drive.PathOnHost = placeholder
		}

		set.drives = append(set.drives, drive)
	}

	return set, nil
}

// check validates one bound volume against what the snapshot recorded and
// computes the size the VM will see. recorded is the slot's size in the
// snapshot's device table.
func (b *boundWorkspace) check(mode workspaceMode, recorded uint64) error {
	if b.ExpectedFsUUID != "" && b.ExpectedFsUUID != b.fsUUID {
		return fmt.Errorf("%w: slot %d incarnation has ext4 UUID %s, the control plane recorded %s", ErrWorkspaceRestore, b.Slot, b.fsUUID, b.ExpectedFsUUID)
	}

	if b.ExpectedCapacityBytes != 0 && b.ExpectedCapacityBytes != b.imageBytes {
		return fmt.Errorf("%w: slot %d incarnation is %d bytes, the control plane recorded %d", ErrWorkspaceRestore, b.Slot, b.imageBytes, b.ExpectedCapacityBytes)
	}

	if b.ResizeToBytes != 0 {
		if b.ResizeToBytes < b.imageBytes {
			return fmt.Errorf("%w: slot %d resize to %d bytes would shrink a %d byte volume", ErrWorkspaceRestore, b.Slot, b.ResizeToBytes, b.imageBytes)
		}

		b.sizeBytes = b.ResizeToBytes
	}

	if mode == workspaceColdBoot {
		// A fresh kernel sizes every drive from its file and mounts from scratch:
		// no identity or size is cached anywhere.
		return nil
	}

	if b.revision != nil {
		// The guest kernel in this memory image has the revision mounted: its
		// superblock, UUID and checksum seed are cached in RAM.
		if b.revision.Consistency != string(blockvol.ConsistencyPairedMemory) {
			return fmt.Errorf("%w: slot %d revision is %q, a memory restore needs %q", ErrWorkspaceRestore, b.Slot, b.revision.Consistency, blockvol.ConsistencyPairedMemory)
		}

		if b.fsUUID != b.revision.FsUUID {
			return fmt.Errorf("%w: slot %d incarnation has ext4 UUID %s, the snapshot's revision %s has %s", ErrWorkspaceRestore, b.Slot, b.fsUUID, b.revision.RevisionID, b.revision.FsUUID)
		}

		if b.ResizeToBytes == 0 && b.imageBytes != recorded {
			return fmt.Errorf("%w: slot %d incarnation is %d bytes, the snapshot recorded %d and no resize was requested", ErrWorkspaceRestore, b.Slot, b.imageBytes, recorded)
		}

		if b.imageBytes < recorded {
			return fmt.Errorf("%w: slot %d incarnation is %d bytes, smaller than the %d the guest has mounted", ErrWorkspaceRestore, b.Slot, b.imageBytes, recorded)
		}

		// Any requested resize is completed by the guest, online: the host must
		// not rewrite a superblock the restored kernel caches.
		b.onlineResize = b.ResizeToBytes != 0

		return nil
	}

	// No revision: the guest never had this slot mounted (a template base
	// snapshot held a placeholder there), so the device may differ from the
	// placeholder size as long as it is not smaller; FC is told to re-read it
	// after the load. The filesystem was made at imageBytes, so only a grow
	// past that needs envd to resize after it mounts the volume.
	if b.sizeBytes < recorded {
		return fmt.Errorf("%w: slot %d volume is %d bytes, smaller than the slot's %d byte reservation", ErrWorkspaceRestore, b.Slot, b.sizeBytes, recorded)
	}

	b.onlineResize = b.sizeBytes > b.imageBytes

	return nil
}

// prepareColdBoot runs the host-side work a cold boot needs before any VM
// opens the volumes: an offline grow of the image and its filesystem. Only
// called in workspaceColdBoot mode, before admission (blockvol refuses an
// offline resize of a held incarnation).
func (w *workspaceSet) prepareColdBoot(ctx context.Context) error {
	if w == nil || w.mode != workspaceColdBoot {
		return nil
	}

	for _, b := range w.bound {
		if b.sizeBytes == b.imageBytes {
			continue
		}

		if err := w.backend.GrowFilesystemOffline(ctx, b.ref, b.sizeBytes); err != nil {
			return fmt.Errorf("offline resize of workspace slot %d: %w", b.Slot, err)
		}
	}

	return nil
}

// admit is the fc.WorkspaceAdmission hook: it records pid as the single
// writer of every bound incarnation and, on a memory restore, grows images
// that are to be resized (the device; the guest grows the filesystem).
func (w *workspaceSet) admit(ctx context.Context, pid int) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	for _, b := range w.bound {
		holder, err := w.backend.Bind(ctx, blockvol.BindRequest{
			Incarnation: b.ref,
			SandboxID:   w.sandboxID,
			ExecutionID: w.executionID,
			FCPid:       pid,
		})
		if err != nil {
			return fmt.Errorf("bind workspace slot %d: %w", b.Slot, err)
		}

		b.holder = &holder

		if w.mode == workspaceMemoryRestore && b.sizeBytes > b.imageBytes {
			if err := w.backend.GrowImage(ctx, b.ref, w.executionID, b.sizeBytes); err != nil {
				return fmt.Errorf("grow workspace slot %d to %d bytes: %w", b.Slot, b.sizeBytes, err)
			}
		}
	}

	return nil
}

// release gives the holder records back once the Firecracker process is
// proven gone. exited resolves when the process has exited. On timeout the
// records stay: a VM that may still write must not have its volume rebound.
func (w *workspaceSet) release(ctx context.Context, exited <-chan struct{}) error {
	if w == nil {
		return nil
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	var held []*boundWorkspace
	for _, b := range w.bound {
		if b.holder != nil {
			held = append(held, b)
		}
	}

	if len(held) == 0 {
		return nil
	}

	wait := workspaceReleaseWait
	if w.releaseWait > 0 {
		wait = w.releaseWait
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-exited:
	case <-timer.C:
		return fmt.Errorf("firecracker of sandbox %s did not exit within %s; keeping %d workspace holder record(s)", w.sandboxID, wait, len(held))
	}

	var errs []error
	for _, b := range held {
		err := w.backend.Release(ctx, blockvol.ReleaseRequest{Incarnation: b.ref.Incarnation, ExecutionID: w.executionID})
		if err != nil {
			errs = append(errs, fmt.Errorf("release workspace slot %d: %w", b.Slot, err))

			continue
		}

		b.holder = nil
	}

	return errors.Join(errs...)
}

// hasVolumes reports whether any slot is bound to a real volume (placeholders
// alone do not make a sandbox subject to the block-volume restrictions).
func (w *workspaceSet) hasVolumes() bool {
	return w != nil && len(w.bound) > 0
}

// slots returns the bound slot indices, ascending.
func (w *workspaceSet) slots() []int {
	if w == nil {
		return nil
	}

	out := make([]int, 0, len(w.bound))
	for _, b := range w.bound {
		out = append(out, b.Slot)
	}

	return out
}

// sealIDs derives the revision and operation ids of one slot's seal from the
// pause's build id. Deterministic, so a replayed pause of the same build hits
// blockvol's operation record instead of producing a second revision.
func sealIDs(buildID uuid.UUID, slot int) (op blockvol.ID, rev blockvol.ID) {
	op = blockvol.MustID(uuid.NewSHA1(buildID, []byte("workspace-seal/"+strconv.Itoa(slot))).String())
	rev = blockvol.MustID(uuid.NewSHA1(buildID, []byte("workspace-revision/"+strconv.Itoa(slot))).String())

	return op, rev
}

// seal takes a read-only revision of every bound incarnation. The caller
// guarantees the VM is paused and CreateSnapshot has drained and fsynced every
// drive; consistency records which protocol produced that state.
func (w *workspaceSet) seal(ctx context.Context, buildID uuid.UUID, consistency blockvol.Consistency) ([]SealedWorkspace, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	out := make([]SealedWorkspace, 0, len(w.bound))
	for _, b := range w.bound {
		if b.holder == nil {
			return nil, fmt.Errorf("seal workspace slot %d: this execution holds no admission", b.Slot)
		}

		op, rev := sealIDs(buildID, b.Slot)

		info, err := w.backend.Seal(ctx, op, b.ref, rev, w.executionID)
		if err != nil {
			return nil, fmt.Errorf("seal workspace slot %d: %w", b.Slot, err)
		}

		if consistency == blockvol.ConsistencyPairedMemory && info.FilesystemUUID != b.fsUUID {
			return nil, fmt.Errorf("seal workspace slot %d: revision has ext4 UUID %s, the running guest mounted %s", b.Slot, info.FilesystemUUID, b.fsUUID)
		}

		out = append(out, SealedWorkspace{
			Slot:          b.Slot,
			VolumeID:      b.VolumeID,
			IncarnationID: b.IncarnationID,
			OperationID:   op.String(),
			Revision: metadata.WorkspaceRevisionRef{
				Slot:        b.Slot,
				RevisionID:  info.Ref.Revision.String(),
				SubvolUUID:  info.SubvolumeUUID,
				FsUUID:      info.FilesystemUUID,
				SizeBytes:   int64(info.CapacityBytes), //nolint:gosec // image sizes fit in int64
				Consistency: string(consistency),
			},
		})
	}

	return out, nil
}

// stampWorkspaceMetadata records a pause's revisions in the snapshot metadata
// and re-stamps the slot sizes: a slot grown since the template was built now
// has a different size in the device table this snapshot carries, and every
// later restore (bound or placeholder) must present exactly that size.
func stampWorkspaceMetadata(m metadata.Template, seals []SealedWorkspace) metadata.Template {
	m.WorkspaceRevisions = nil
	if len(seals) == 0 {
		return m
	}

	if m.Workspace != nil {
		ws := *m.Workspace
		ws.SlotCapacityBytes = slices.Clone(ws.SlotCapacityBytes)

		for _, s := range seals {
			if s.Slot < len(ws.SlotCapacityBytes) {
				ws.SlotCapacityBytes[s.Slot] = s.Revision.SizeBytes
			}
		}

		m.Workspace = &ws
	}

	m.WorkspaceRevisions = make([]metadata.WorkspaceRevisionRef, 0, len(seals))
	for _, s := range seals {
		m.WorkspaceRevisions = append(m.WorkspaceRevisions, s.Revision)
	}

	slices.SortFunc(m.WorkspaceRevisions, func(a, b metadata.WorkspaceRevisionRef) int { return cmp.Compare(a.Slot, b.Slot) })

	return m
}

// envdMounts are the typed block mounts envd /init gets for the bound slots.
// envd finds each device by ext4 UUID and checks its size.
func (w *workspaceSet) envdMounts() []envd.VolumeMount {
	if w == nil {
		return nil
	}

	out := make([]envd.VolumeMount, 0, len(w.bound))
	for _, b := range w.bound {
		out = append(out, envd.VolumeMount{
			SourceType:    envd.Block,
			Path:          b.MountPath,
			FsUuid:        b.fsUUID,
			CapacityBytes: int64(b.sizeBytes), //nolint:gosec // image sizes fit in int64
			DeviceId:      fc.WorkspaceDriveID(b.Slot),
			FormatVersion: WorkspaceFormatVersion,
		})
	}

	return out
}

// onlineResizePaths are the guest mount paths whose filesystem must be grown
// after envd mounted (or re-attached) them.
func (w *workspaceSet) onlineResizePaths() []string {
	if w == nil {
		return nil
	}

	var out []string
	for _, b := range w.bound {
		if b.onlineResize {
			out = append(out, b.MountPath)
		}
	}

	return out
}

// installWorkspace wires a planned set into a Firecracker process and the
// sandbox cleanup: the process admits itself before opening the volumes, and
// the holders are released only after it has exited.
func installWorkspace(ctx context.Context, ws *workspaceSet, proc *fc.Process, cleanup *Cleanup) {
	if ws == nil {
		return
	}

	proc.SetWorkspaceAdmission(ws.admit)

	cleanup.Add(ctx, func(ctx context.Context) error {
		err := ws.release(ctx, proc.Exit.Done())
		if err != nil {
			logger.L().Error(ctx, "workspace volumes not released", zap.Error(err), logger.WithSandboxID(ws.sandboxID))
		}

		return err
	})
}

// WorkspaceSlots returns the workspace slots this sandbox has real volumes
// bound to, ascending. Empty for a sandbox without workspace volumes.
func (s *Sandbox) WorkspaceSlots() []int {
	return s.workspace.slots()
}

// HasWorkspaceVolumes reports whether this sandbox has workspace block volumes
// bound, and so is subject to their lifecycle restrictions.
func (s *Sandbox) HasWorkspaceVolumes() bool {
	return s.workspace.hasVolumes()
}

// CheckPauseSupported refuses, with ErrCapabilityUnsupported, a pause whose
// options a sandbox with workspace volumes cannot take. Pause calls it first;
// handlers that tear the sandbox down on any pause error call it before doing
// so, so a refusal leaves the sandbox running.
func (s *Sandbox) CheckPauseSupported(opts ...PauseOption) error {
	var o pauseOptions
	for _, opt := range opts {
		opt(&o)
	}

	return s.checkPauseSupported(o)
}

func (s *Sandbox) checkPauseSupported(o pauseOptions) error {
	if !s.workspace.hasVolumes() {
		return nil
	}

	switch {
	case o.resumeFresh:
		// A resume-fresh checkpoint loads the new snapshot into a second VM
		// while the first still holds the volumes; the first release has no
		// hand-over between the two writers.
		return fmt.Errorf("%w: resume-fresh checkpoint (use an in-place checkpoint)", ErrCapabilityUnsupported)
	case o.deferWorkspaceSeal:
		return fmt.Errorf("%w: deferred workspace seal (the seal must complete before the VM resumes)", ErrCapabilityUnsupported)
	}

	return nil
}

// sealWorkspaceForPause seals every bound volume of a paused sandbox, after
// CreateSnapshot and before any resume (see Pause). A filesystem-only pause
// reaches here only after the whole mount set froze, so its revisions are
// filesystem-quiesced; a memory pause's pair with the memory image.
func (s *Sandbox) sealWorkspaceForPause(ctx context.Context, buildID uuid.UUID, o pauseOptions) ([]SealedWorkspace, error) {
	if !s.workspace.hasVolumes() {
		return nil, nil
	}

	consistency := blockvol.ConsistencyPairedMemory
	if o.filesystemSnapshot {
		consistency = blockvol.ConsistencyFilesystemQuiesced
	}

	ctx, span := tracer.Start(ctx, "workspace-seal")
	defer span.End()

	seals, err := s.workspace.seal(ctx, buildID, consistency)
	if err != nil {
		return nil, fmt.Errorf("workspace seal: %w", err)
	}

	return seals, nil
}

// requireWorkspaceSealed fails unless seals covers every bound slot. It guards
// the in-place resume: the guest must not run again before its volumes are
// sealed for the snapshot being taken.
func (s *Sandbox) requireWorkspaceSealed(seals []SealedWorkspace) error {
	for _, slot := range s.workspace.slots() {
		if !slices.ContainsFunc(seals, func(w SealedWorkspace) bool { return w.Slot == slot && w.Revision.RevisionID != "" }) {
			return fmt.Errorf("%w: slot %d, refusing to resume before the workspace seal", ErrWorkspaceNotSealed, slot)
		}
	}

	return nil
}

// resizeWorkspaceFilesystems asks envd to grow the mounted filesystem of every
// slot that was grown for this start. It runs after envd /init.
func (s *Sandbox) resizeWorkspaceFilesystems(ctx context.Context) error {
	for _, p := range s.workspace.onlineResizePaths() {
		if err := s.callEnvdVolumeResize(ctx, p); err != nil {
			return fmt.Errorf("online resize of workspace volume at %s: %w", p, err)
		}
	}

	return nil
}
