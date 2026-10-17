//go:build linux

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/blockvol"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/envd"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/fc"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/metadata"
)

// fakeWorkspaceBackend is a WorkspaceBackend over an in-memory set of
// incarnations. Every call is appended to an event log shared with the test,
// so orderings across the backend and the fake VM can be asserted.
type fakeWorkspaceBackend struct {
	mu      sync.Mutex
	log     *[]string
	incs    map[string]blockvol.IncarnationInfo // by incarnation id
	holders map[string]string                   // incarnation id -> execution id
	failOn  map[string]error                    // "seal" / "bind" / "grow"
}

func newFakeWorkspaceBackend(log *[]string) *fakeWorkspaceBackend {
	return &fakeWorkspaceBackend{log: log, incs: map[string]blockvol.IncarnationInfo{}, holders: map[string]string{}, failOn: map[string]error{}}
}

func (f *fakeWorkspaceBackend) record(format string, args ...any) {
	*f.log = append(*f.log, fmt.Sprintf(format, args...))
}

func (f *fakeWorkspaceBackend) add(team, volume blockvol.ID, fsUUID string, size uint64) blockvol.IncarnationRef {
	ref := blockvol.IncarnationRef{VolumeRef: blockvol.VolumeRef{Team: team, Volume: volume}, Incarnation: blockvol.NewID()}
	f.incs[ref.Incarnation.String()] = blockvol.IncarnationInfo{
		Ref:            ref,
		SubvolumeUUID:  uuid.NewString(),
		FilesystemUUID: fsUUID,
		ImagePath:      "/vol/" + ref.Incarnation.String() + "/workspace.ext4",
		CapacityBytes:  size,
	}

	return ref
}

func (f *fakeWorkspaceBackend) Bind(_ context.Context, req blockvol.BindRequest) (blockvol.Holder, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.record("bind %s pid=%d", req.Incarnation.Incarnation, req.FCPid)
	if err := f.failOn["bind"]; err != nil {
		return blockvol.Holder{}, err
	}

	if cur, ok := f.holders[req.Incarnation.Incarnation.String()]; ok && cur != req.ExecutionID {
		return blockvol.Holder{}, errors.New("held")
	}

	f.holders[req.Incarnation.Incarnation.String()] = req.ExecutionID

	return blockvol.Holder{Incarnation: req.Incarnation.Incarnation, ExecutionID: req.ExecutionID, FCPid: req.FCPid}, nil
}

func (f *fakeWorkspaceBackend) Release(_ context.Context, req blockvol.ReleaseRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.record("release %s", req.Incarnation)
	delete(f.holders, req.Incarnation.String())

	return nil
}

func (f *fakeWorkspaceBackend) Seal(_ context.Context, op blockvol.ID, inc blockvol.IncarnationRef, revision blockvol.ID, executionID string) (blockvol.RevisionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.record("seal %s rev=%s op=%s", inc.Incarnation, revision, op)
	if err := f.failOn["seal"]; err != nil {
		return blockvol.RevisionInfo{}, err
	}

	if f.holders[inc.Incarnation.String()] != executionID {
		return blockvol.RevisionInfo{}, blockvol.ErrHolderMismatch
	}

	info := f.incs[inc.Incarnation.String()]

	return blockvol.RevisionInfo{
		Ref:               blockvol.RevisionRef{VolumeRef: inc.VolumeRef, Revision: revision},
		SourceIncarnation: inc.Incarnation,
		SourceOperation:   op,
		SubvolumeUUID:     "subvol-" + revision.String(),
		FilesystemUUID:    info.FilesystemUUID,
		CapacityBytes:     info.CapacityBytes,
	}, nil
}

func (f *fakeWorkspaceBackend) DescribeIncarnation(_ context.Context, inc blockvol.IncarnationRef) (blockvol.IncarnationInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	info, ok := f.incs[inc.Incarnation.String()]
	if !ok || info.Ref.VolumeRef != inc.VolumeRef {
		return blockvol.IncarnationInfo{}, fmt.Errorf("%w: %s", blockvol.ErrIncarnationNotFound, inc.Incarnation)
	}

	return info, nil
}

func (f *fakeWorkspaceBackend) GrowImage(_ context.Context, inc blockvol.IncarnationRef, executionID string, size uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.record("grow-image %s to=%d", inc.Incarnation, size)
	if err := f.failOn["grow"]; err != nil {
		return err
	}

	if f.holders[inc.Incarnation.String()] != executionID {
		return blockvol.ErrHolderMismatch
	}

	info := f.incs[inc.Incarnation.String()]
	info.CapacityBytes = size
	f.incs[inc.Incarnation.String()] = info

	return nil
}

func (f *fakeWorkspaceBackend) GrowFilesystemOffline(_ context.Context, inc blockvol.IncarnationRef, size uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.record("grow-offline %s to=%d", inc.Incarnation, size)
	if _, held := f.holders[inc.Incarnation.String()]; held {
		return blockvol.ErrHolderMismatch
	}

	info := f.incs[inc.Incarnation.String()]
	info.CapacityBytes = size
	f.incs[inc.Incarnation.String()] = info

	return nil
}

func (f *fakeWorkspaceBackend) Placeholder(_ context.Context, capacity uint64) (string, error) {
	return fmt.Sprintf("/vol/placeholders/%d.img", capacity), nil
}

const (
	gib     = uint64(1 << 30)
	slotCap = int64(gib)
)

type workspaceFixture struct {
	log     []string
	backend *fakeWorkspaceBackend
	team    blockvol.ID
	volume  blockvol.ID
	fsUUID  string
	inc     blockvol.IncarnationRef
}

func newWorkspaceFixture(t *testing.T) *workspaceFixture {
	t.Helper()

	fx := &workspaceFixture{team: blockvol.NewID(), volume: blockvol.NewID(), fsUUID: uuid.NewString()}
	fx.backend = newFakeWorkspaceBackend(&fx.log)
	fx.inc = fx.backend.add(fx.team, fx.volume, fx.fsUUID, gib)

	return fx
}

func (fx *workspaceFixture) volumeFor(slot int, inc blockvol.IncarnationRef) WorkspaceVolume {
	return WorkspaceVolume{Slot: slot, VolumeID: inc.Volume.String(), IncarnationID: inc.Incarnation.String(), MountPath: fmt.Sprintf("/workspace%d", slot)}
}

func blockTemplate(slots int, revs ...metadata.WorkspaceRevisionRef) metadata.Template {
	caps := make([]int64, slots)
	for i := range caps {
		caps[i] = slotCap
	}

	return metadata.Template{
		Layout:             metadata.LayoutBlockV1,
		Workspace:          &metadata.WorkspaceLayout{Slots: slots, SlotCapacityBytes: caps},
		WorkspaceRevisions: revs,
	}
}

func (fx *workspaceFixture) plan(t *testing.T, meta metadata.Template, volumes []WorkspaceVolume, mode workspaceMode) (*workspaceSet, error) {
	t.Helper()

	return planWorkspace(t.Context(), fx.backend, meta, volumes, fx.team.String(), "sbx-1", "lifecycle-1", mode)
}

func pairedRevision(slot int, fsUUID string, size int64) metadata.WorkspaceRevisionRef {
	return metadata.WorkspaceRevisionRef{Slot: slot, RevisionID: uuid.NewString(), SubvolUUID: "s", FsUUID: fsUUID, SizeBytes: size, Consistency: string(blockvol.ConsistencyPairedMemory)}
}

func TestPlanWorkspace_LayoutGate(t *testing.T) {
	t.Parallel()

	fx := newWorkspaceFixture(t)
	vol := fx.volumeFor(0, fx.inc)

	ws, err := fx.plan(t, metadata.Template{}, nil, workspaceMemoryRestore)
	require.NoError(t, err)
	assert.Nil(t, ws, "a legacy template without volumes needs no workspace")

	for name, tc := range map[string]struct {
		meta    metadata.Template
		volumes []WorkspaceVolume
		want    error
	}{
		"legacy template":       {meta: metadata.Template{}, volumes: []WorkspaceVolume{vol}, want: ErrWorkspaceLayout},
		"slot outside layout":   {meta: blockTemplate(1), volumes: []WorkspaceVolume{fx.volumeFor(1, fx.inc)}, want: ErrWorkspaceLayout},
		"negative slot":         {meta: blockTemplate(2), volumes: []WorkspaceVolume{fx.volumeFor(-1, fx.inc)}, want: ErrWorkspaceLayout},
		"slot bound twice":      {meta: blockTemplate(2), volumes: []WorkspaceVolume{vol, {Slot: 0, VolumeID: vol.VolumeID, IncarnationID: vol.IncarnationID, MountPath: "/other"}}, want: ErrWorkspaceLayout},
		"mount path twice":      {meta: blockTemplate(2), volumes: []WorkspaceVolume{vol, {Slot: 1, VolumeID: vol.VolumeID, IncarnationID: vol.IncarnationID, MountPath: vol.MountPath}}, want: ErrWorkspaceLayout},
		"relative mount path":   {meta: blockTemplate(1), volumes: []WorkspaceVolume{{Slot: 0, VolumeID: vol.VolumeID, IncarnationID: vol.IncarnationID, MountPath: "work"}}, want: ErrWorkspaceLayout},
		"root mount path":       {meta: blockTemplate(1), volumes: []WorkspaceVolume{{Slot: 0, VolumeID: vol.VolumeID, IncarnationID: vol.IncarnationID, MountPath: "/"}}, want: ErrWorkspaceLayout},
		"bad incarnation id":    {meta: blockTemplate(1), volumes: []WorkspaceVolume{{Slot: 0, VolumeID: vol.VolumeID, IncarnationID: "x", MountPath: "/w"}}, want: ErrWorkspaceLayout},
		"missing incarnation":   {meta: blockTemplate(1), volumes: []WorkspaceVolume{{Slot: 0, VolumeID: vol.VolumeID, IncarnationID: uuid.NewString(), MountPath: "/w"}}, want: ErrWorkspaceRestore},
		"too many slots in tpl": {meta: blockTemplate(fc.MaxWorkspaceSlots + 1), volumes: []WorkspaceVolume{vol}, want: ErrWorkspaceLayout},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := fx.plan(t, tc.meta, tc.volumes, workspaceMemoryRestore)
			require.ErrorIs(t, err, tc.want)
		})
	}

	_, err = planWorkspace(t.Context(), nil, blockTemplate(1), nil, fx.team.String(), "sbx", "lc", workspaceMemoryRestore)
	require.ErrorIs(t, err, ErrCapabilityUnsupported, "a block-layout template on a node without a backend is refused, not started without slots")
}

func TestPlanWorkspace_DrivesCoverEverySlot(t *testing.T) {
	t.Parallel()

	fx := newWorkspaceFixture(t)

	ws, err := fx.plan(t, blockTemplate(3), []WorkspaceVolume{fx.volumeFor(1, fx.inc)}, workspaceMemoryRestore)
	require.NoError(t, err)

	require.Len(t, ws.drives, 3, "the snapshot's device table names every reserved slot")
	assert.Equal(t, fc.WorkspaceDrive{DriveID: "workspace0", PathOnHost: "/vol/placeholders/1073741824.img"}, ws.drives[0])
	assert.Equal(t, fc.WorkspaceDrive{DriveID: "workspace1", PathOnHost: "/vol/" + fx.inc.Incarnation.String() + "/workspace.ext4"}, ws.drives[1])
	assert.Equal(t, "workspace2", ws.drives[2].DriveID)
	assert.Equal(t, []int{1}, ws.slots())
	assert.Empty(t, fx.log, "planning binds nothing")

	mounts := ws.envdMounts()
	require.Len(t, mounts, 1)
	assert.Equal(t, envd.VolumeMount{
		SourceType:    envd.Block,
		Path:          "/workspace1",
		FsUuid:        fx.fsUUID,
		CapacityBytes: int64(gib),
		DeviceId:      "workspace1",
		FormatVersion: WorkspaceFormatVersion,
	}, mounts[0])
}

func TestPlanWorkspace_RestoreValidation(t *testing.T) {
	t.Parallel()

	fx := newWorkspaceFixture(t)
	vol := fx.volumeFor(0, fx.inc)

	t.Run("a mounted slot without a volume is refused", func(t *testing.T) {
		t.Parallel()

		_, err := fx.plan(t, blockTemplate(2, pairedRevision(1, fx.fsUUID, slotCap)), []WorkspaceVolume{vol}, workspaceMemoryRestore)
		require.ErrorIs(t, err, ErrWorkspaceRestore)
	})

	t.Run("paired-memory needs the revision's ext4 UUID", func(t *testing.T) {
		t.Parallel()

		_, err := fx.plan(t, blockTemplate(1, pairedRevision(0, uuid.NewString(), slotCap)), []WorkspaceVolume{vol}, workspaceMemoryRestore)
		require.ErrorIs(t, err, ErrWorkspaceRestore)

		ws, err := fx.plan(t, blockTemplate(1, pairedRevision(0, fx.fsUUID, slotCap)), []WorkspaceVolume{vol}, workspaceMemoryRestore)
		require.NoError(t, err)
		assert.False(t, ws.drives[0].RefreshSize)
	})

	t.Run("a filesystem-quiesced revision cannot be memory-restored", func(t *testing.T) {
		t.Parallel()

		rev := pairedRevision(0, fx.fsUUID, slotCap)
		rev.Consistency = string(blockvol.ConsistencyFilesystemQuiesced)
		_, err := fx.plan(t, blockTemplate(1, rev), []WorkspaceVolume{vol}, workspaceMemoryRestore)
		require.ErrorIs(t, err, ErrWorkspaceRestore)
	})

	t.Run("capacity must match without a resize", func(t *testing.T) {
		t.Parallel()

		meta := blockTemplate(1, pairedRevision(0, fx.fsUUID, slotCap))
		meta.Workspace.SlotCapacityBytes[0] = 2 * slotCap
		_, err := fx.plan(t, meta, []WorkspaceVolume{vol}, workspaceMemoryRestore)
		require.ErrorIs(t, err, ErrWorkspaceRestore)
	})

	t.Run("a resize never shrinks", func(t *testing.T) {
		t.Parallel()

		shrink := vol
		shrink.ResizeToBytes = gib / 2
		_, err := fx.plan(t, blockTemplate(1, pairedRevision(0, fx.fsUUID, slotCap)), []WorkspaceVolume{shrink}, workspaceMemoryRestore)
		require.ErrorIs(t, err, ErrWorkspaceRestore)

		_, err = fx.plan(t, blockTemplate(1), []WorkspaceVolume{shrink}, workspaceColdBoot)
		require.ErrorIs(t, err, ErrWorkspaceRestore)
	})

	t.Run("the control plane's record must match the image", func(t *testing.T) {
		t.Parallel()

		wrong := vol
		wrong.ExpectedFsUUID = uuid.NewString()
		_, err := fx.plan(t, blockTemplate(1), []WorkspaceVolume{wrong}, workspaceColdBoot)
		require.ErrorIs(t, err, ErrWorkspaceRestore)

		wrong = vol
		wrong.ExpectedCapacityBytes = 2 * gib
		_, err = fx.plan(t, blockTemplate(1), []WorkspaceVolume{wrong}, workspaceColdBoot)
		require.ErrorIs(t, err, ErrWorkspaceRestore)
	})

	t.Run("a cold boot does not need the revision's UUID", func(t *testing.T) {
		t.Parallel()

		rev := pairedRevision(0, uuid.NewString(), slotCap)
		rev.Consistency = string(blockvol.ConsistencyFilesystemQuiesced)
		ws, err := fx.plan(t, blockTemplate(1, rev), []WorkspaceVolume{vol}, workspaceColdBoot)
		require.NoError(t, err)
		assert.False(t, ws.drives[0].RefreshSize, "a cold boot sizes drives from their files")
		assert.Empty(t, ws.onlineResizePaths())
	})

	t.Run("a fresh volume larger than the placeholder is presented at its size", func(t *testing.T) {
		t.Parallel()

		big := fx.backend.add(fx.team, fx.volume, uuid.NewString(), 2*gib)
		ws, err := fx.plan(t, blockTemplate(1), []WorkspaceVolume{fx.volumeFor(0, big)}, workspaceMemoryRestore)
		require.NoError(t, err)
		assert.True(t, ws.drives[0].RefreshSize, "FC re-reads the size after loading a placeholder-sized device table")
		assert.Empty(t, ws.onlineResizePaths(), "the filesystem already spans the image")

		small := fx.backend.add(fx.team, fx.volume, uuid.NewString(), gib/2)
		_, err = fx.plan(t, blockTemplate(1), []WorkspaceVolume{fx.volumeFor(0, small)}, workspaceMemoryRestore)
		require.ErrorIs(t, err, ErrWorkspaceRestore)
	})
}

// TestWorkspace_PairedMemoryResizeOrdering pins §7.5 for a memory restore:
// bind first (the gate), then grow the image as the holder, then FC loads and
// re-reads the size (RefreshSize), then envd grows the mounted ext4 online.
// The host never runs resize2fs on a volume the restored guest has mounted.
func TestWorkspace_PairedMemoryResizeOrdering(t *testing.T) {
	t.Parallel()

	fx := newWorkspaceFixture(t)
	vol := fx.volumeFor(0, fx.inc)
	vol.ResizeToBytes = 3 * gib

	ws, err := fx.plan(t, blockTemplate(1, pairedRevision(0, fx.fsUUID, slotCap)), []WorkspaceVolume{vol}, workspaceMemoryRestore)
	require.NoError(t, err)
	assert.True(t, ws.drives[0].RefreshSize)
	assert.Equal(t, []string{"/workspace0"}, ws.onlineResizePaths())

	require.NoError(t, ws.prepareColdBoot(t.Context()), "no offline work on a memory restore")
	require.NoError(t, ws.admit(t.Context(), 4242))

	inc := fx.inc.Incarnation.String()
	assert.Equal(t, []string{
		"bind " + inc + " pid=4242",
		fmt.Sprintf("grow-image %s to=%d", inc, 3*gib),
	}, fx.log)
	assert.Equal(t, int64(3*gib), ws.envdMounts()[0].CapacityBytes)
}

// TestWorkspace_ColdBootResizeIsOffline pins §7.5 for a cold boot: the image
// and its ext4 are grown on the host while nobody holds the incarnation, and
// only then is the VM admitted.
func TestWorkspace_ColdBootResizeIsOffline(t *testing.T) {
	t.Parallel()

	fx := newWorkspaceFixture(t)
	vol := fx.volumeFor(0, fx.inc)
	vol.ResizeToBytes = 2 * gib

	ws, err := fx.plan(t, blockTemplate(1), []WorkspaceVolume{vol}, workspaceColdBoot)
	require.NoError(t, err)
	assert.Empty(t, ws.onlineResizePaths())

	require.NoError(t, ws.prepareColdBoot(t.Context()))
	require.NoError(t, ws.admit(t.Context(), 7))

	inc := fx.inc.Incarnation.String()
	assert.Equal(t, []string{
		fmt.Sprintf("grow-offline %s to=%d", inc, 2*gib),
		"bind " + inc + " pid=7",
	}, fx.log, "offline resize strictly before admission, and no online grow")
}

func TestWorkspace_SealAndStamp(t *testing.T) {
	t.Parallel()

	fx := newWorkspaceFixture(t)
	other := fx.backend.add(fx.team, blockvol.NewID(), uuid.NewString(), 2*gib)
	meta := blockTemplate(3)
	meta.Workspace.SlotCapacityBytes[2] = 2 * slotCap

	ws, err := fx.plan(t, meta, []WorkspaceVolume{fx.volumeFor(2, other), fx.volumeFor(0, fx.inc)}, workspaceMemoryRestore)
	require.NoError(t, err)

	buildID := uuid.New()

	_, err = ws.seal(t.Context(), buildID, blockvol.ConsistencyPairedMemory)
	require.Error(t, err, "nothing to seal before this execution was admitted")

	require.NoError(t, ws.admit(t.Context(), 1))

	seals, err := ws.seal(t.Context(), buildID, blockvol.ConsistencyPairedMemory)
	require.NoError(t, err)
	require.Len(t, seals, 2)
	assert.Equal(t, 0, seals[0].Slot, "slot order")
	assert.Equal(t, 2, seals[1].Slot)

	op0, rev0 := sealIDs(buildID, 0)
	assert.Equal(t, op0.String(), seals[0].OperationID)
	assert.Equal(t, rev0.String(), seals[0].Revision.RevisionID)
	assert.Equal(t, fx.fsUUID, seals[0].Revision.FsUUID)
	assert.Equal(t, string(blockvol.ConsistencyPairedMemory), seals[0].Revision.Consistency)
	assert.Equal(t, other.Incarnation.String(), seals[1].IncarnationID)

	again, err := ws.seal(t.Context(), buildID, blockvol.ConsistencyPairedMemory)
	require.NoError(t, err)
	assert.Equal(t, seals[0].Revision.RevisionID, again[0].Revision.RevisionID, "a replayed pause of the same build reuses the ids")

	_, otherRev := sealIDs(uuid.New(), 0)
	assert.NotEqual(t, rev0, otherRev)

	stamped := stampWorkspaceMetadata(meta, seals)
	assert.Equal(t, []metadata.WorkspaceRevisionRef{seals[0].Revision, seals[1].Revision}, stamped.WorkspaceRevisions)
	assert.Equal(t, []int64{slotCap, slotCap, 2 * slotCap}, stamped.Workspace.SlotCapacityBytes)
	assert.Equal(t, []int64{slotCap, slotCap, 2 * slotCap}, meta.Workspace.SlotCapacityBytes, "the input metadata is not mutated")

	cleared := stampWorkspaceMetadata(stamped, nil)
	assert.Empty(t, cleared.WorkspaceRevisions)
}

func TestWorkspace_SealStampsGrownSize(t *testing.T) {
	t.Parallel()

	fx := newWorkspaceFixture(t)
	vol := fx.volumeFor(0, fx.inc)
	vol.ResizeToBytes = 2 * gib
	meta := blockTemplate(1, pairedRevision(0, fx.fsUUID, slotCap))

	ws, err := fx.plan(t, meta, []WorkspaceVolume{vol}, workspaceMemoryRestore)
	require.NoError(t, err)
	require.NoError(t, ws.admit(t.Context(), 1))

	seals, err := ws.seal(t.Context(), uuid.New(), blockvol.ConsistencyPairedMemory)
	require.NoError(t, err)

	stamped := stampWorkspaceMetadata(meta, seals)
	assert.Equal(t, []int64{2 * slotCap}, stamped.Workspace.SlotCapacityBytes, "the next restore presents the grown size")
}

func TestWorkspace_SealFailureAndHolderMismatch(t *testing.T) {
	t.Parallel()

	fx := newWorkspaceFixture(t)
	ws, err := fx.plan(t, blockTemplate(1), []WorkspaceVolume{fx.volumeFor(0, fx.inc)}, workspaceMemoryRestore)
	require.NoError(t, err)
	require.NoError(t, ws.admit(t.Context(), 1))

	fx.backend.failOn["seal"] = errors.New("btrfs: no space")
	_, err = ws.seal(t.Context(), uuid.New(), blockvol.ConsistencyPairedMemory)
	require.ErrorContains(t, err, "no space")
	delete(fx.backend.failOn, "seal")

	// The image no longer carries the UUID the guest mounted: a paired seal of
	// it would pair the memory image with a foreign filesystem.
	info := fx.backend.incs[fx.inc.Incarnation.String()]
	info.FilesystemUUID = uuid.NewString()
	fx.backend.incs[fx.inc.Incarnation.String()] = info
	_, err = ws.seal(t.Context(), uuid.New(), blockvol.ConsistencyPairedMemory)
	require.ErrorContains(t, err, "ext4 UUID")
}

func TestWorkspace_ReleaseWaitsForProcessExit(t *testing.T) {
	t.Parallel()

	fx := newWorkspaceFixture(t)
	ws, err := fx.plan(t, blockTemplate(1), []WorkspaceVolume{fx.volumeFor(0, fx.inc)}, workspaceMemoryRestore)
	require.NoError(t, err)

	never := make(chan struct{})
	require.NoError(t, ws.release(t.Context(), never), "nothing bound, nothing to wait for")

	require.NoError(t, ws.admit(t.Context(), 1))

	ws.releaseWait = 20 * time.Millisecond
	err = ws.release(t.Context(), never)
	require.ErrorContains(t, err, "did not exit")
	assert.Contains(t, fx.backend.holders, fx.inc.Incarnation.String(), "a VM not proven gone keeps its volume")

	exited := make(chan struct{})
	close(exited)
	require.NoError(t, ws.release(t.Context(), exited))
	assert.NotContains(t, fx.backend.holders, fx.inc.Incarnation.String())
	assert.Equal(t, "release "+fx.inc.Incarnation.String(), fx.log[len(fx.log)-1])

	require.NoError(t, ws.release(t.Context(), exited), "idempotent")
}

func newWorkspaceSandbox(t *testing.T, fx *workspaceFixture, envdURL string) *Sandbox {
	t.Helper()

	s := newFsFreezeSandbox(t, envdURL)
	ws, err := fx.plan(t, blockTemplate(1), []WorkspaceVolume{fx.volumeFor(0, fx.inc)}, workspaceMemoryRestore)
	require.NoError(t, err)
	require.NoError(t, ws.admit(t.Context(), 1))
	s.workspace = ws

	return s
}

// TestPause_RefusalsAreRaisedInsidePause drives Sandbox.Pause itself: the
// refusal fires before anything is touched (the sandbox under test has no FC
// process at all, so any later step would panic), which is what lets every
// caller of Pause — not only the gRPC handlers — rely on it.
func TestPause_RefusalsAreRaisedInsidePause(t *testing.T) {
	t.Parallel()

	fx := newWorkspaceFixture(t)
	s := newWorkspaceSandbox(t, fx, "http://127.0.0.1:1")

	for name, opt := range map[string]PauseOption{
		"resume-fresh checkpoint": WithResumeFreshCheckpoint(),
		"deferred workspace seal": WithDeferredWorkspaceSeal(),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := s.Pause(t.Context(), metadata.Template{}, SnapshotUseCasePause, WithMaintainSandbox(), opt)
			require.ErrorIs(t, err, ErrCapabilityUnsupported)
			require.ErrorIs(t, s.CheckPauseSupported(opt), ErrCapabilityUnsupported)
		})
	}

	require.NoError(t, s.CheckPauseSupported(WithMaintainSandbox()), "an in-place checkpoint is supported")
	require.NoError(t, s.CheckPauseSupported(WithFilesystemSnapshot()))

	plain := newFsFreezeSandbox(t, "http://127.0.0.1:1")
	require.NoError(t, plain.CheckPauseSupported(WithResumeFreshCheckpoint(), WithDeferredWorkspaceSeal()), "no workspace volumes, no restriction")
	assert.Empty(t, fx.log[1:], "nothing was sealed or released by a refused pause")
}

func TestRequireWorkspaceSealed(t *testing.T) {
	t.Parallel()

	fx := newWorkspaceFixture(t)
	s := newWorkspaceSandbox(t, fx, "http://127.0.0.1:1")

	require.ErrorIs(t, s.requireWorkspaceSealed(nil), ErrWorkspaceNotSealed)

	seals, err := s.sealWorkspaceForPause(t.Context(), uuid.New(), pauseOptions{maintainSandbox: true})
	require.NoError(t, err)
	require.NoError(t, s.requireWorkspaceSealed(seals))
	assert.Equal(t, string(blockvol.ConsistencyPairedMemory), seals[0].Revision.Consistency)

	fsSeals, err := s.sealWorkspaceForPause(t.Context(), uuid.New(), pauseOptions{filesystemSnapshot: true})
	require.NoError(t, err)
	assert.Equal(t, string(blockvol.ConsistencyFilesystemQuiesced), fsSeals[0].Revision.Consistency)

	plain := newFsFreezeSandbox(t, "http://127.0.0.1:1")
	none, err := plain.sealWorkspaceForPause(t.Context(), uuid.New(), pauseOptions{})
	require.NoError(t, err)
	assert.Empty(t, none)
	require.NoError(t, plain.requireWorkspaceSealed(nil))
}

func TestSnapshotWorkspaceReady(t *testing.T) {
	t.Parallel()

	rev := pairedRevision(0, uuid.NewString(), slotCap)
	seal := SealedWorkspace{Slot: 0, Revision: rev}

	require.NoError(t, (&Snapshot{}).WorkspaceReady(), "no workspace volumes")
	require.ErrorIs(t, (&Snapshot{workspaceSlots: []int{0}}).WorkspaceReady(), ErrWorkspaceNotSealed)
	require.ErrorIs(t, (&Snapshot{workspaceSlots: []int{0}, WorkspaceSeals: []SealedWorkspace{seal}}).WorkspaceReady(), ErrWorkspaceNotSealed, "the metadata must carry the revision too")
	require.ErrorIs(t, (&Snapshot{workspaceSlots: []int{0, 1}, WorkspaceSeals: []SealedWorkspace{seal}, WorkspaceRevisions: []metadata.WorkspaceRevisionRef{rev}}).WorkspaceReady(), ErrWorkspaceNotSealed)
	require.NoError(t, (&Snapshot{workspaceSlots: []int{0}, WorkspaceSeals: []SealedWorkspace{seal}, WorkspaceRevisions: []metadata.WorkspaceRevisionRef{rev}}).WorkspaceReady())

	incomplete := seal
	incomplete.Revision.SubvolUUID = ""
	require.ErrorIs(t, (&Snapshot{workspaceSlots: []int{0}, WorkspaceSeals: []SealedWorkspace{incomplete}, WorkspaceRevisions: []metadata.WorkspaceRevisionRef{incomplete.Revision}}).WorkspaceReady(), ErrWorkspaceNotSealed)
}
