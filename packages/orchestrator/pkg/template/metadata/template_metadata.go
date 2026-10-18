package metadata

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"

	"go.opentelemetry.io/otel"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block"
	"github.com/e2b-dev/infra/packages/shared/pkg/consts"
	"github.com/e2b-dev/infra/packages/shared/pkg/ioutils"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
)

const (
	CurrentVersion = 2

	DeprecatedVersion = 1

	// FilesystemOnlyVersion is the metadata version that introduced the
	// filesystem_only field. A filesystem-only snapshot must be stamped at least
	// this version so deserialize() (which strips every field from a version <=
	// DeprecatedVersion snapshot) keeps the flag. It is pinned rather than
	// CurrentVersion: CurrentVersion advances as the format grows, and stamping a
	// minimal V1-derived snapshot with a newer version would falsely imply it
	// carries later-version fields.
	FilesystemOnlyVersion = DeprecatedVersion + 1
)

// ErrReplaceCommitted means the replacement is visible, but parent-directory
// durability could not be confirmed.
var ErrReplaceCommitted = ioutils.ErrAtomicWriteCommitted

const (
	// LayoutLegacy is the device layout every template had before workspace block
	// slots: a rootfs drive and nothing else. It is the empty string so that
	// pre-existing metadata (no field) decodes to it without a migration.
	LayoutLegacy = ""

	// LayoutBlockV1 is the layout whose base snapshot reserves workspace block
	// slots next to the rootfs (see Template.Workspace). Also the capability name
	// a reader must have to handle such a template.
	LayoutBlockV1 = "block-v1"
)

// KnownCapabilities are the layouts and required capabilities this reader
// handles. A Template naming anything outside this set is refused by the decode
// path (ErrUnsupportedLayout) so that a reader built before a layout existed
// never restores, forks or deletes what it does not understand. Adding a layout
// means adding it here, in the release that can handle it, BEFORE any writer may
// produce it.
var KnownCapabilities = map[string]struct{}{
	LayoutBlockV1:             {},
	CapabilityMemoryHotplugV1: {},
}

// ErrUnsupportedLayout is the sentinel every UnsupportedLayoutError matches with
// errors.Is. It is returned by the metadata decode path (FromFile, FromBuildID),
// which is the one place both the API and the orchestrator read snapshots
// through, so the gate cannot be bypassed by reading from a different entry.
var ErrUnsupportedLayout = errors.New("unsupported template layout")

// UnsupportedLayoutError reports which layout or required capability this reader
// does not know. It wraps ErrUnsupportedLayout for errors.Is.
type UnsupportedLayoutError struct {
	// Layout is the unknown layout, or "" when only a capability is unknown.
	Layout string
	// Capabilities are the required capabilities this reader lacks.
	Capabilities []string
}

func (e *UnsupportedLayoutError) Error() string {
	switch {
	case e.Layout != "" && len(e.Capabilities) > 0:
		return fmt.Sprintf("%s: layout %q, required capabilities %v", ErrUnsupportedLayout, e.Layout, e.Capabilities)
	case e.Layout != "":
		return fmt.Sprintf("%s: layout %q", ErrUnsupportedLayout, e.Layout)
	default:
		return fmt.Sprintf("%s: required capabilities %v", ErrUnsupportedLayout, e.Capabilities)
	}
}

func (e *UnsupportedLayoutError) Is(target error) bool {
	return target == ErrUnsupportedLayout
}

func (e *UnsupportedLayoutError) Unwrap() error {
	return ErrUnsupportedLayout
}

var tracer = otel.Tracer("github.com/e2b-dev/infra/packages/orchestrator/pkg/template/metadata")

// AccessType is a compact representation of block access type for JSON serialization.
type AccessType string

const (
	AccessRead     AccessType = "r"
	AccessWrite    AccessType = "w"
	AccessPrefetch AccessType = "p"
)

// blockToAccessType maps block.AccessType to metadata.AccessType.
var blockToAccessType = map[block.AccessType]AccessType{
	block.Read:     AccessRead,
	block.Write:    AccessWrite,
	block.Prefetch: AccessPrefetch,
}

type Version struct {
	Version any `json:"version"`
}

type Context struct {
	User    string            `json:"user,omitempty"`
	WorkDir *string           `json:"workdir,omitempty"`
	EnvVars map[string]string `json:"env_vars,omitempty"`
}

// EnvdDefaultUser reports the DefaultUser this template's build sent to envd's /init, and
// whether the metadata can establish it at all.
//
// Only one recorded value can be established, and the reason is that determinacy here is a
// property of the build's TEMPLATE VERSION while the metadata records a USER:
//
//   - finalize sends Context.User for a build at or above TemplateV2ReleaseVersion, and a
//     flat ("user", nil) for anything below it, discarding whatever was recorded.
//   - Context.User is written by three things at three different gates: the base phase
//     stamps "root" unconditionally for every from-image build; the USER phase overwrites
//     it with "user", gated on that same version; and a user-authored USER step overwrites
//     it with any name at ANY version, because only the USER phase is gated and the step
//     builders are appended unconditionally.
//
// So a recorded "app" is either a recent build whose /init received "app", or an older one
// whose /init received "user" — and the build's template version is not carried in the
// snapshot (Template.Version is the metadata FORMAT version). Same ambiguity for "root".
// Only consts.TemplateDefaultUser is safe, because both branches send exactly that.
//
// No workdir is reported for the same reason and it is not an oversight: finalize sends
// Context.WorkDir above the version gate and nil below it, so a recorded workdir is
// unprovable by the same argument. Re-sending one would move the working directory of
// every command that omits a cwd, on every start, for a template that never had it.
func (t Template) EnvdDefaultUser() (user string, ok bool) {
	if t.Context.User != consts.TemplateDefaultUser {
		return "", false
	}

	return t.Context.User, true
}

func (c Context) WithUser(user string) Context {
	c.User = user

	return c
}

type FromTemplate struct {
	Alias   string `json:"alias"`
	BuildID string `json:"build_id"`
}

type Start struct {
	StartCmd string  `json:"start_command"`
	ReadyCmd string  `json:"ready_command"`
	Context  Context `json:"context"`
}

type TemplateMetadata struct {
	BuildID            string `json:"build_id"`
	KernelVersion      string `json:"kernel_version"`
	FirecrackerVersion string `json:"firecracker_version"`
}

// MemoryPrefetchMapping stores block offsets that should be prefetched when starting a sandbox.
// This is used to speed up sandbox starts by proactively fetching blocks that are likely to be needed.
type MemoryPrefetchMapping struct {
	// Indices is an ordered array of block indices to prefetch, preserving the order in which blocks were accessed
	Indices []uint64 `json:"indices"`
	// AccessTypes stores the access type ("r"/"w"/"p") for each block, aligned with Indices
	AccessTypes []AccessType `json:"access_types"`
	// BlockSize is the size of each block in bytes
	BlockSize int64 `json:"block_size"`
}

// AccessTypeFromBlock converts a block.AccessType to metadata.AccessType.
func AccessTypeFromBlock(at block.AccessType) AccessType {
	return blockToAccessType[at]
}

// Count returns the number of blocks to prefetch.
func (p *MemoryPrefetchMapping) Count() int {
	if p == nil {
		return 0
	}

	return len(p.Indices)
}

type Prefetch struct {
	Memory *MemoryPrefetchMapping `json:"memory"`
}

// WorkspaceLayout describes the workspace block slots a block-layout template
// reserved in its device table at build time.
type WorkspaceLayout struct {
	// Slots is the number of workspace drives in the device table, numbered from
	// 0 after the rootfs.
	Slots int `json:"slots"`
	// SlotCapacityBytes is the size of each slot's backing file, index-aligned
	// with the slots. A snapshot restores only with files of exactly this size
	// per slot, so it is recorded rather than re-derived.
	SlotCapacityBytes []int64 `json:"slot_capacity_bytes"`
}

// WorkspaceRevisionRef points a snapshot at the sealed revision of one
// workspace slot's volume, as it was at the pause that produced the snapshot.
type WorkspaceRevisionRef struct {
	// Slot is the workspace slot (drive index after the rootfs) the revision
	// was sealed from and restores into.
	Slot int `json:"slot"`
	// RevisionID is the control-plane identity of the sealed revision.
	RevisionID string `json:"revision_id"`
	// SubvolUUID and FsUUID identify the sealed Btrfs snapshot and the
	// filesystem it lives on, so a restore can verify it opens the revision the
	// snapshot was paired with rather than "the latest file" of the volume.
	SubvolUUID string `json:"subvol_uuid"`
	FsUUID     string `json:"fs_uuid"`
	// SizeBytes is the revision's backing-file size; a restore must present the
	// slot at exactly this size.
	SizeBytes int64 `json:"size_bytes"`
	// Consistency is how the revision pairs with the rest of the snapshot:
	// "paired-memory" for a revision sealed with a memory snapshot,
	// "filesystem-quiesced" for one sealed with a frozen filesystem-only pause.
	Consistency string `json:"consistency"`
}

type Template struct {
	Version      uint64           `json:"version"`
	Template     TemplateMetadata `json:"template"`
	Context      Context          `json:"context"`
	Start        *Start           `json:"start,omitempty"`
	FromImage    *string          `json:"from_image,omitempty"`
	FromTemplate *FromTemplate    `json:"from_template,omitempty"`
	Prefetch     *Prefetch        `json:"prefetch,omitempty"`

	// CmdlineArgs records the extra guest kernel command line parameters this template's
	// kernel booted with, parsed and normalised. Nil is the default command line, which is
	// what every pre-existing template has.
	//
	// It stores the PARAMETERS, not the flag value they came from, and that is the point:
	// the flag is edited freely, so what it says can change after this template was built.
	// A cold boot that re-read the flag would silently reproduce a different command line
	// than the build used. Storing the parsed parameters makes a snapshot self-describing.
	//
	// It sits at the top level, alongside Context and Start, because it is inherited lineage
	// state rather than per-build identity: chosen once, at build, with no source to
	// re-derive it from. The nested TemplateMetadata is the wrong home for exactly that
	// reason — SameVersionTemplate replaces that struct wholesale on every pause, so a field
	// living there would be dropped by the most frequently executed path in the system.
	//
	// It exists so the cold boot of a filesystem-only snapshot can re-apply the same command
	// line; a memory resume restores a running kernel and never re-reads it.
	CmdlineArgs map[string]string `json:"cmdline_args,omitempty"`

	// FilesystemOnly marks a snapshot that persists only the filesystem (no
	// memory snapshot); resuming it must cold-boot (reboot) from the rootfs. The
	// zero value (false) is a full memory snapshot, so pre-existing snapshots
	// (no field) are correctly treated as memory without a migration. Stamped at
	// pause time; it is the resume path's source of truth for
	// reboot-vs-memory-resume. Deliberately NOT carried by the
	// With*/SameVersionTemplate copy-constructors — Sandbox.Pause re-stamps it.
	FilesystemOnly bool `json:"filesystem_only,omitempty"`

	// FsQuiesced records that a filesystem-only snapshot's rootfs was captured
	// while frozen (FIFREEZE — native for envd >= 0.6.6, or via the exec API for
	// older envd) rather than merely sync'd, so it is crash-consistent. The zero
	// value (false) means unknown / not-frozen — a legacy snapshot (no field) or a
	// sync fallback. Only meaningful alongside FilesystemOnly. Stamped at pause and
	// re-stamped every pause; deliberately NOT carried by the copy-constructors.
	// This is a building block: it lets a later feature decide safely whether a
	// snapshot is one it can cold-boot / rewrite without repairing the journal.
	FsQuiesced bool `json:"fs_quiesced,omitempty"`

	// Layout names the guest device layout this template's snapshots carry:
	// LayoutLegacy (rootfs only, the zero value every pre-existing template
	// decodes to) or LayoutBlockV1 (rootfs plus Workspace slots).
	//
	// Top level, next to CmdlineArgs, for the same reason: the device table is
	// fixed when the base snapshot is taken and every later snapshot of the
	// lineage restores it, so it is inherited lineage state. It must survive
	// SameVersionTemplate on every pause, which the nested TemplateMetadata does
	// not. A reader that does not know the layout refuses the whole Template
	// (see KnownCapabilities).
	Layout string `json:"layout,omitempty"`

	// Workspace describes the workspace block slots of a LayoutBlockV1 template;
	// nil for LayoutLegacy. Carried like Layout.
	Workspace *WorkspaceLayout `json:"workspace,omitempty"`

	// RequiredCapabilities lists what a reader must support to handle this
	// Template beyond what Layout implies. Empty for every template so far; the
	// decode path refuses any entry outside KnownCapabilities. Carried like
	// Layout: a capability a snapshot needs, every later snapshot of the lineage
	// needs too.
	RequiredCapabilities []string `json:"required_capabilities,omitempty"`

	// WorkspaceRevisions are the sealed workspace volume revisions paired with
	// this snapshot, one per bound slot, stamped at pause by the checkpoint
	// path. Like FilesystemOnly it describes ONE snapshot, not the lineage, so
	// SameVersionTemplate/NewVersionTemplate/BasedOn deliberately do NOT carry
	// it and Pause re-stamps it. WithPrefetch does carry it: it rewrites the
	// metadata of the same snapshot, whose revisions are unchanged.
	WorkspaceRevisions []WorkspaceRevisionRef `json:"workspace_revisions,omitempty"`

	// MemoryHotplug is the virtio-mem device of a template built with a
	// maximum memory above its boot memory; nil for every other template (no
	// device, memory fixed at boot size). Lineage state carried like Layout,
	// and always paired with CapabilityMemoryHotplugV1 in RequiredCapabilities
	// (see WithMemoryHotplug) so a reader without hotplug support refuses it.
	MemoryHotplug *MemoryHotplugLayout `json:"memory_hotplug,omitempty"`

	// MemoryHotplugPluggedMiB is the hotplug size that was plugged when this
	// snapshot was taken; a memory resume restores exactly that much before
	// any resize. Describes ONE snapshot (stamped at pause like
	// FilesystemOnly), so only WithPrefetch carries it.
	MemoryHotplugPluggedMiB int64 `json:"memory_hotplug_plugged_mib,omitempty"`
}

// HasWorkspaceSlots reports whether this template's device table reserves
// workspace block slots, i.e. a sandbox of it can bind workspace volumes.
func (t Template) HasWorkspaceSlots() bool {
	return t.Layout == LayoutBlockV1 && t.Workspace != nil && t.Workspace.Slots > 0
}

// checkCapabilities refuses a Template whose layout or required capabilities
// this reader does not know. Run in the single decode path so every consumer
// (API, orchestrator, build) gets the same answer.
func (t Template) checkCapabilities() error {
	var unknown UnsupportedLayoutError

	if _, ok := KnownCapabilities[t.Layout]; t.Layout != LayoutLegacy && !ok {
		unknown.Layout = t.Layout
	}

	for _, c := range t.RequiredCapabilities {
		if _, ok := KnownCapabilities[c]; !ok && !slices.Contains(unknown.Capabilities, c) {
			unknown.Capabilities = append(unknown.Capabilities, c)
		}
	}

	if unknown.Layout == "" && len(unknown.Capabilities) == 0 {
		return nil
	}

	return &unknown
}

// IsFilesystemOnly reports whether this snapshot persists only the filesystem
// (no memory snapshot), so resuming it must cold-boot (reboot) from the rootfs.
func (t Template) IsFilesystemOnly() bool {
	return t.FilesystemOnly
}

// MarkFilesystemOnly records whether this snapshot persists only the filesystem.
//
// When marking it filesystem-only, the metadata version is upgraded to at least
// FilesystemOnlyVersion if needed: deserialize() strips every field (including
// filesystem_only) from a snapshot whose version is <= DeprecatedVersion, so a
// snapshot taken from a V1 template would otherwise lose the flag on resume and
// be wrongly memory-resumed — and since the filesystem-only pause uploaded no
// memory snapshot, that resume hard-fails. Clearing the flag never changes the
// version.
func (t Template) MarkFilesystemOnly(filesystemOnly bool) Template {
	t.FilesystemOnly = filesystemOnly
	if filesystemOnly && t.Version < FilesystemOnlyVersion {
		t.Version = FilesystemOnlyVersion
	}

	return t
}

// IsFsQuiesced reports whether a filesystem-only snapshot's rootfs was frozen
// (crash-consistent) at pause.
func (t Template) IsFsQuiesced() bool {
	return t.FsQuiesced
}

// MarkFsQuiesced records whether the rootfs was frozen at pause. It is only set
// true together with FilesystemOnly, whose MarkFilesystemOnly already lifts the
// metadata version to >= FilesystemOnlyVersion; deserialize() fully unmarshals
// those versions, so the flag survives without a further version bump.
func (t Template) MarkFsQuiesced(quiesced bool) Template {
	t.FsQuiesced = quiesced

	return t
}

func V1TemplateVersion() Template {
	return Template{
		Version: 1,
	}
}

// BasedOn derives the metadata of a build that starts FROM another template.
//
// Deliberately does NOT carry CmdlineArgs. Unlike a pause, which continues one
// lineage, this is a new build that resolves the guest kernel cmdline flag for its own
// team — inheriting the parent's arguments would record arguments the child's kernel
// never booted with, and a later filesystem-only cold boot would then apply them.
//
// It DOES carry Layout, Workspace, RequiredCapabilities and MemoryHotplug: the child build resumes
// the parent's snapshot, whose device table it cannot change, so the slots are a
// fact about the child's layers too. The finalize phase then stamps the build's
// own configuration over them, as it does for CmdlineArgs.
func (t Template) BasedOn(
	ft FromTemplate,
) Template {
	return Template{
		Version:              CurrentVersion,
		Template:             t.Template,
		Context:              t.Context,
		Start:                t.Start,
		FromTemplate:         &ft,
		FromImage:            nil,
		Layout:               t.Layout,
		Workspace:            t.Workspace,
		RequiredCapabilities: t.RequiredCapabilities,
		MemoryHotplug:        t.MemoryHotplug.Clone(),
	}
}

func (t Template) NewVersionTemplate(metadata TemplateMetadata) Template {
	return Template{
		Version:              CurrentVersion,
		Template:             metadata,
		Context:              t.Context,
		Start:                t.Start,
		FromTemplate:         t.FromTemplate,
		FromImage:            t.FromImage,
		CmdlineArgs:          t.CmdlineArgs,
		Layout:               t.Layout,
		Workspace:            t.Workspace,
		RequiredCapabilities: t.RequiredCapabilities,
		MemoryHotplug:        t.MemoryHotplug.Clone(),
	}
}

func (t Template) SameVersionTemplate(metadata TemplateMetadata) Template {
	return Template{
		Version:              t.Version,
		Template:             metadata,
		Context:              t.Context,
		Start:                t.Start,
		FromTemplate:         t.FromTemplate,
		FromImage:            t.FromImage,
		CmdlineArgs:          t.CmdlineArgs,
		Layout:               t.Layout,
		Workspace:            t.Workspace,
		RequiredCapabilities: t.RequiredCapabilities,
		MemoryHotplug:        t.MemoryHotplug.Clone(),
	}
}

// WithPrefetch returns a copy of the template with the given prefetch mapping.
// It describes the SAME snapshot, so unlike the other copy-constructors it keeps
// WorkspaceRevisions.
func (t Template) WithPrefetch(prefetch *Prefetch) Template {
	return Template{
		Version:                 t.Version,
		Template:                t.Template,
		Context:                 t.Context,
		Start:                   t.Start,
		FromTemplate:            t.FromTemplate,
		FromImage:               t.FromImage,
		Prefetch:                prefetch,
		CmdlineArgs:             t.CmdlineArgs,
		Layout:                  t.Layout,
		Workspace:               t.Workspace,
		RequiredCapabilities:    t.RequiredCapabilities,
		WorkspaceRevisions:      t.WorkspaceRevisions,
		MemoryHotplug:           t.MemoryHotplug.Clone(),
		MemoryHotplugPluggedMiB: t.MemoryHotplugPluggedMiB,
	}
}

func (t Template) ToFile(path string) error {
	mr, err := serialize(t)
	if err != nil {
		return err
	}

	err = ioutils.WriteToFileFromReader(path, mr)
	if err != nil {
		return fmt.Errorf("failed to write metadata to file: %w", err)
	}

	return nil
}

func (t Template) ReplaceFile(path string) error {
	mr, err := serialize(t)
	if err != nil {
		return err
	}

	err = ioutils.WriteToFileFromReaderAtomically(path, mr)
	if err != nil {
		return fmt.Errorf("failed to replace metadata file: %w", err)
	}

	return nil
}

func FromFile(path string) (Template, error) {
	f, err := os.Open(path)
	if err != nil {
		return Template{}, fmt.Errorf("failed to open metadata file: %w", err)
	}
	defer f.Close()

	templateMetadata, err := deserialize(f)
	if err != nil {
		return Template{}, fmt.Errorf("failed to deserialize metadata: %w", err)
	}

	return templateMetadata, nil
}

func FromBuildID(ctx context.Context, s storage.StorageProvider, buildID string) (Template, error) {
	return fromTemplate(ctx, s, storage.Paths{
		BuildID: buildID,
	})
}

func fromTemplate(ctx context.Context, s storage.StorageProvider, paths storage.Paths) (Template, error) {
	ctx, span := tracer.Start(ctx, "from template")
	defer span.End()

	obj, err := s.OpenBlob(ctx, paths.Metadata())
	if err != nil {
		return Template{}, fmt.Errorf("error opening object for template metadata: %w", err)
	}

	data, err := storage.GetBlob(ctx, obj)
	if err != nil {
		return Template{}, fmt.Errorf("error reading template metadata from object: %w", err)
	}

	templateMetadata, err := deserialize(bytes.NewReader(data))
	if err != nil {
		return Template{}, err
	}

	return templateMetadata, nil
}

func deserialize(reader io.Reader) (Template, error) {
	// Read all data into bytes first to avoid double stream read
	data, err := io.ReadAll(reader)
	if err != nil {
		return Template{}, fmt.Errorf("error reading template metadata: %w", err)
	}

	var v Version
	err = json.Unmarshal(data, &v)
	if err != nil {
		return Template{}, fmt.Errorf("error unmarshaling template version: %w", err)
	}

	// Handle deprecated version formats gracefully
	// When any type is used, Go will unmarshal any numeric value as a float64
	if version, ok := v.Version.(float64); !ok || version <= float64(DeprecatedVersion) {
		return Template{Version: DeprecatedVersion}, nil
	}

	var templateMetadata Template
	err = json.Unmarshal(data, &templateMetadata)
	if err != nil {
		return Template{}, fmt.Errorf("error unmarshaling template metadata: %w", err)
	}

	// Unknown JSON fields are ignored above, so a newer writer's additions are
	// invisible to an older reader. Layout and RequiredCapabilities are the
	// fields a writer sets precisely to make a reader notice; refuse rather
	// than treat the snapshot as the legacy layout it is not.
	if err := templateMetadata.checkCapabilities(); err != nil {
		return Template{}, err
	}

	// A hotplug layout sizes the memory file and the UFFD handshake; a
	// malformed one must not reach a resume.
	if l := templateMetadata.MemoryHotplug; l != nil {
		if err := l.Validate(); err != nil {
			return Template{}, err
		}
	}

	return templateMetadata, nil
}

// serialize serializes a template to a reader for uploading.
func serialize(template Template) (io.Reader, error) {
	marshaled, err := json.Marshal(template)
	if err != nil {
		return nil, fmt.Errorf("error serializing template metadata: %w", err)
	}

	buf := bytes.NewBuffer(marshaled)

	return buf, nil
}
