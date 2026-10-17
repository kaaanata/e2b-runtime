// Package blockvol implements the host-side backend for workspace block
// volumes: Btrfs subvolumes holding a fixed-capacity ext4 image that is
// attached to a Firecracker VM as a dedicated drive.
//
// The package is only called from inside the orchestrator. Every physical
// path is derived from validated UUIDs through Layout; callers never pass
// host paths in.
package blockvol

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

const (
	// RootEnv is the environment variable that overrides DefaultRoot.
	RootEnv = "BLOCK_VOLUME_ROOT"
	// DefaultRoot is the Btrfs mount that holds all workspace volumes.
	DefaultRoot = "/var/lib/e2b/volumes"

	// ImageName is the ext4 image inside every incarnation and revision subvolume.
	ImageName = "workspace.ext4"

	teamsDir        = "teams"
	incarnationsDir = "inc"
	revisionsDir    = "rev"
	holdersDir      = "holders"
	stagingDir      = "staging"
	placeholdersDir = "placeholders"
	holderSuffix    = ".json"
	holderLockFile  = ".lock"
)

// ErrInvalidID is returned (wrapped) when a caller-supplied identifier is not
// a canonical lowercase RFC 4122 UUID string.
var ErrInvalidID = errors.New("blockvol: identifier is not a canonical UUID")

// ID is a validated identifier. The zero value is invalid; obtain one through
// ParseID (or MustID in tests).
type ID struct { //nolint:recvcheck // encoding.TextUnmarshaler needs a pointer receiver; everything else is by value
	s string
}

// ParseID accepts only the canonical 36-character lowercase form so that the
// string can be used as a path component without any further escaping.
func ParseID(s string) (ID, error) {
	if len(s) != 36 {
		return ID{}, fmt.Errorf("%w: %q", ErrInvalidID, s)
	}

	parsed, err := uuid.Parse(s)
	if err != nil {
		return ID{}, fmt.Errorf("%w: %q: %w", ErrInvalidID, s, err)
	}

	if parsed == uuid.Nil || parsed.String() != s {
		return ID{}, fmt.Errorf("%w: %q", ErrInvalidID, s)
	}

	return ID{s: s}, nil
}

// MustID panics on invalid input; intended for tests and constants.
func MustID(s string) ID {
	id, err := ParseID(s)
	if err != nil {
		panic(err)
	}

	return id
}

// NewID returns a fresh random ID.
func NewID() ID {
	return ID{s: uuid.NewString()}
}

func (id ID) String() string { return id.s }

// IsZero reports whether the ID was never parsed.
func (id ID) IsZero() bool { return id.s == "" }

// UUID returns the parsed form; the ID is always valid so this cannot fail.
func (id ID) UUID() uuid.UUID { return uuid.MustParse(id.s) }

func (id ID) MarshalText() ([]byte, error) {
	if id.IsZero() {
		return nil, ErrInvalidID
	}

	return []byte(id.s), nil
}

func (id *ID) UnmarshalText(b []byte) error {
	parsed, err := ParseID(string(b))
	if err != nil {
		return err
	}

	*id = parsed

	return nil
}

// VolumeRef names a logical volume owned by a team.
type VolumeRef struct {
	Team   ID
	Volume ID
}

// IncarnationRef names one writable physicalisation of a volume.
type IncarnationRef struct {
	VolumeRef

	Incarnation ID
}

// RevisionRef names one sealed (read-only) snapshot of a volume.
type RevisionRef struct {
	VolumeRef

	Revision ID
}

func (v VolumeRef) validate() error {
	if v.Team.IsZero() || v.Volume.IsZero() {
		return fmt.Errorf("%w: volume reference has zero id", ErrInvalidID)
	}

	return nil
}

func (r IncarnationRef) validate() error {
	if err := r.VolumeRef.validate(); err != nil {
		return err
	}

	if r.Incarnation.IsZero() {
		return fmt.Errorf("%w: incarnation reference has zero id", ErrInvalidID)
	}

	return nil
}

func (r RevisionRef) validate() error {
	if err := r.VolumeRef.validate(); err != nil {
		return err
	}

	if r.Revision.IsZero() {
		return fmt.Errorf("%w: revision reference has zero id", ErrInvalidID)
	}

	return nil
}

// Layout maps validated identifiers to the physical directory tree:
//
//	<root>/
//	  teams/<team>/<volume>/
//	    inc/<incarnation>/workspace.ext4   writable subvolume
//	    rev/<revision>/workspace.ext4      read-only snapshot
//	  holders/<incarnation>.json           writer admission record (not snapshotted)
//	  holders/<incarnation>.lock           flock anchor for the record
//	  staging/<operation>/                 unpublished artefacts of one operation
//	  placeholders/<capacity-bytes>.img    sparse slot placeholders
//
// The whole tree must live on one Btrfs filesystem so that staging artefacts
// can be published with rename(2).
type Layout struct {
	root string
}

// NewLayout returns a Layout rooted at root. An empty root selects
// $BLOCK_VOLUME_ROOT, falling back to DefaultRoot.
func NewLayout(root string) (Layout, error) {
	if root == "" {
		root = os.Getenv(RootEnv)
	}

	if root == "" {
		root = DefaultRoot
	}

	abs, err := filepath.Abs(root)
	if err != nil {
		return Layout{}, fmt.Errorf("blockvol: resolve root %q: %w", root, err)
	}

	return Layout{root: abs}, nil
}

// Root is the absolute Btrfs root directory.
func (l Layout) Root() string { return l.root }

func (l Layout) TeamsDir() string        { return filepath.Join(l.root, teamsDir) }
func (l Layout) HoldersDir() string      { return filepath.Join(l.root, holdersDir) }
func (l Layout) StagingRoot() string     { return filepath.Join(l.root, stagingDir) }
func (l Layout) PlaceholdersDir() string { return filepath.Join(l.root, placeholdersDir) }

func (l Layout) TeamDir(team ID) string {
	return filepath.Join(l.TeamsDir(), team.String())
}

func (l Layout) VolumeDir(v VolumeRef) string {
	return filepath.Join(l.TeamDir(v.Team), v.Volume.String())
}

func (l Layout) IncarnationsDir(v VolumeRef) string {
	return filepath.Join(l.VolumeDir(v), incarnationsDir)
}

func (l Layout) RevisionsDir(v VolumeRef) string {
	return filepath.Join(l.VolumeDir(v), revisionsDir)
}

// IncarnationDir is the writable subvolume of one incarnation.
func (l Layout) IncarnationDir(r IncarnationRef) string {
	return filepath.Join(l.IncarnationsDir(r.VolumeRef), r.Incarnation.String())
}

// IncarnationImage is the ext4 image handed to Firecracker as the backing file.
func (l Layout) IncarnationImage(r IncarnationRef) string {
	return filepath.Join(l.IncarnationDir(r), ImageName)
}

// RevisionDir is the read-only snapshot subvolume of one revision.
func (l Layout) RevisionDir(r RevisionRef) string {
	return filepath.Join(l.RevisionsDir(r.VolumeRef), r.Revision.String())
}

func (l Layout) RevisionImage(r RevisionRef) string {
	return filepath.Join(l.RevisionDir(r), ImageName)
}

// HolderPath is the JSON admission record of an incarnation.
func (l Layout) HolderPath(incarnation ID) string {
	return filepath.Join(l.HoldersDir(), incarnation.String()+holderSuffix)
}

// HolderLockPath is the never-replaced file whose flock serialises updates of
// the JSON record within one orchestrator process.
func (l Layout) HolderLockPath(incarnation ID) string {
	return filepath.Join(l.HoldersDir(), incarnation.String()+holderLockFile)
}

// StagingDir holds unpublished artefacts of one operation.
func (l Layout) StagingDir(operation ID) string {
	return filepath.Join(l.StagingRoot(), operation.String())
}

// PlaceholderImage is the shared sparse image for one capacity tier.
func (l Layout) PlaceholderImage(capacityBytes uint64) string {
	return filepath.Join(l.PlaceholdersDir(), fmt.Sprintf("%d.img", capacityBytes))
}

// EnsureDirs creates the fixed top-level directories.
func (l Layout) EnsureDirs() error {
	for _, dir := range []string{l.TeamsDir(), l.HoldersDir(), l.StagingRoot(), l.PlaceholdersDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("blockvol: create %s: %w", dir, err)
		}
	}

	return nil
}
