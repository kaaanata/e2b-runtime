package blockvol

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// HeldByError is returned by Bind when another live execution owns the
// incarnation.
type HeldByError struct {
	Holder Holder
}

func (e *HeldByError) Error() string {
	return fmt.Sprintf("blockvol: incarnation %s is held by sandbox %s execution %s (pid %d)", e.Holder.Incarnation, e.Holder.SandboxID, e.Holder.ExecutionID, e.Holder.FCPid)
}

// BlockedError is returned when the recorded holder process is gone but the
// incarnation has not been reclaimed with explicit evidence.
type BlockedError struct {
	Holder Holder
}

func (e *BlockedError) Error() string {
	return fmt.Sprintf("blockvol: incarnation %s is blocked: %s (last holder sandbox %s execution %s)", e.Holder.Incarnation, e.Holder.BlockedReason, e.Holder.SandboxID, e.Holder.ExecutionID)
}

// StillRunningError is returned by Release when the holder process is alive.
type StillRunningError struct {
	Holder Holder
}

func (e *StillRunningError) Error() string {
	return fmt.Sprintf("blockvol: cannot release incarnation %s: holder process %d (starttime %d) is still running", e.Holder.Incarnation, e.Holder.FCPid, e.Holder.FCStarttime)
}

// ErrHolderMismatch is returned when a Release or Seal names an execution
// that is not the recorded holder.
var ErrHolderMismatch = errors.New("blockvol: execution is not the recorded holder")

// ErrReclaimEvidenceMismatch is returned when a reclaim names a holder that is
// not the one on disk; the caller must re-read and decide again.
var ErrReclaimEvidenceMismatch = errors.New("blockvol: reclaim evidence does not match the recorded holder")

// ReclaimEvidence is the caller's acknowledgement of exactly which dead holder
// it is taking over. All three fields must match the record on disk.
type ReclaimEvidence struct {
	ExecutionID string
	FCPid       int
	FCStarttime uint64
	Reason      string
}

// BindRequest asks for physical write admission on behalf of a Firecracker
// process that is already running (its pid must exist at bind time so the
// starttime can be recorded).
type BindRequest struct {
	Incarnation IncarnationRef
	SandboxID   string
	ExecutionID string
	FCPid       int
	// Reclaim, when set, allows taking over a holder whose process is proven
	// gone. Without it a dead holder is only marked blocked.
	Reclaim *ReclaimEvidence
}

// ReleaseRequest gives admission back after the Firecracker process exited.
type ReleaseRequest struct {
	Incarnation ID
	ExecutionID string
}

// Gate is the physical writer admission for incarnations on this node.
//
// Strength boundary: the flock taken on holders/<incarnation>.lock only
// serialises goroutines of this orchestrator process (and dies with it: the
// kernel drops the lock when the last descriptor closes). It is NOT what
// protects the image. The JSON record is the source of truth; after an
// orchestrator crash, startup reclaim must walk holders/, wait for each
// recorded FC process to exit, and either Release it or leave it blocked.
// The database partial-unique index on active attachments is the cross-node
// guard; this gate is the last line on the node that actually opens the file.
type Gate struct {
	layout   Layout
	liveness ProcessLiveness
	bootID   string
	now      func() time.Time
}

// NewGate reads the node boot id from procfs.
func NewGate(layout Layout, liveness ProcessLiveness) (*Gate, error) {
	bootID, err := NodeBootID()
	if err != nil {
		return nil, err
	}

	return newGate(layout, liveness, bootID, time.Now), nil
}

func newGate(layout Layout, liveness ProcessLiveness, bootID string, now func() time.Time) *Gate {
	return &Gate{layout: layout, liveness: liveness, bootID: bootID, now: now}
}

// withLock runs fn while holding the per-incarnation flock. See the Gate
// comment for what this lock does and does not guarantee.
func (g *Gate) withLock(incarnation ID, fn func() error) error {
	if err := os.MkdirAll(g.layout.HoldersDir(), 0o700); err != nil {
		return fmt.Errorf("blockvol: create holders dir: %w", err)
	}

	lock, err := os.OpenFile(g.layout.HolderLockPath(incarnation), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("blockvol: open holder lock: %w", err)
	}
	defer lock.Close()

	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("blockvol: flock holder lock: %w", err)
	}
	defer func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN) }()

	return fn()
}

// Bind admits req.FCPid as the single writer of the incarnation.
//
//   - no record: write one, return it.
//   - record for the same execution and pid: idempotent, return it.
//   - record whose process is alive: HeldByError.
//   - record whose process is gone, no Reclaim: mark blocked, BlockedError.
//   - record blocked or dead, Reclaim matching the record: take over.
func (g *Gate) Bind(ctx context.Context, req BindRequest) (Holder, error) {
	if err := req.Incarnation.validate(); err != nil {
		return Holder{}, err
	}

	if req.ExecutionID == "" || req.SandboxID == "" || req.FCPid <= 0 {
		return Holder{}, errors.New("blockvol: bind requires sandbox id, execution id and a positive pid")
	}

	if err := ctx.Err(); err != nil {
		return Holder{}, err
	}

	var result Holder
	err := g.withLock(req.Incarnation.Incarnation, func() error {
		path := g.layout.HolderPath(req.Incarnation.Incarnation)

		existing, err := readHolder(path)
		if err != nil {
			return err
		}

		starttime, err := g.liveness.Starttime(req.FCPid)
		if err != nil {
			return fmt.Errorf("blockvol: bind requires a running firecracker process: %w", err)
		}

		if existing != nil {
			// A replay is only idempotent for the very same process: matching
			// the pid alone would accept a recycled pid.
			if existing.State == HolderActive && existing.NodeBootID == g.bootID &&
				existing.ExecutionID == req.ExecutionID && existing.FCPid == req.FCPid && existing.FCStarttime == starttime {
				result = *existing

				return nil
			}

			if err := g.admitOverExisting(path, existing, req.Reclaim); err != nil {
				return err
			}
		}

		h := Holder{
			Incarnation: req.Incarnation.Incarnation,
			SandboxID:   req.SandboxID,
			ExecutionID: req.ExecutionID,
			NodeBootID:  g.bootID,
			FCPid:       req.FCPid,
			FCStarttime: starttime,
			BackingPath: g.layout.IncarnationImage(req.Incarnation),
			CreatedAt:   g.now().UTC(),
			State:       HolderActive,
		}

		if err := writeHolder(path, &h); err != nil {
			return err
		}

		result = h

		return nil
	})

	return result, err
}

// admitOverExisting decides whether a new binder may replace the record.
func (g *Gate) admitOverExisting(path string, existing *Holder, reclaim *ReclaimEvidence) error {
	alive, err := g.holderAlive(existing)
	if err != nil {
		return err
	}

	if alive {
		return &HeldByError{Holder: *existing}
	}

	if reclaim == nil {
		if existing.State != HolderBlocked {
			now := g.now().UTC()
			existing.State = HolderBlocked
			existing.BlockedReason = "holder process is gone; awaiting reclaim with evidence"
			existing.BlockedAt = &now

			if err := writeHolder(path, existing); err != nil {
				return err
			}
		}

		return &BlockedError{Holder: *existing}
	}

	if reclaim.ExecutionID != existing.ExecutionID || reclaim.FCPid != existing.FCPid || reclaim.FCStarttime != existing.FCStarttime {
		return fmt.Errorf("%w: evidence names execution %s pid %d starttime %d, record has execution %s pid %d starttime %d",
			ErrReclaimEvidenceMismatch, reclaim.ExecutionID, reclaim.FCPid, reclaim.FCStarttime,
			existing.ExecutionID, existing.FCPid, existing.FCStarttime)
	}

	return nil
}

// holderAlive is true only when the record was written during this boot and
// a process with the recorded pid and starttime exists.
func (g *Gate) holderAlive(h *Holder) (bool, error) {
	if h.NodeBootID != g.bootID {
		return false, nil
	}

	return g.liveness.Alive(h.FCPid, h.FCStarttime)
}

// Release removes the record once the holder process is proven gone. The
// proof is procfs: same boot id and no process with that pid and starttime,
// or a different boot id. A live process yields StillRunningError.
func (g *Gate) Release(ctx context.Context, req ReleaseRequest) error {
	if req.Incarnation.IsZero() {
		return fmt.Errorf("%w: release has zero incarnation id", ErrInvalidID)
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	return g.withLock(req.Incarnation, func() error {
		path := g.layout.HolderPath(req.Incarnation)

		existing, err := readHolder(path)
		if err != nil {
			return err
		}

		if existing == nil {
			return nil
		}

		if req.ExecutionID != "" && existing.ExecutionID != req.ExecutionID {
			return fmt.Errorf("%w: release by %s, record held by %s", ErrHolderMismatch, req.ExecutionID, existing.ExecutionID)
		}

		alive, err := g.holderAlive(existing)
		if err != nil {
			return err
		}

		if alive {
			return &StillRunningError{Holder: *existing}
		}

		return removeHolder(path)
	})
}

// Block marks an incarnation as blocked without a bind attempt; startup
// reclaim uses it when a recorded process cannot be confirmed gone in time.
func (g *Gate) Block(ctx context.Context, incarnation ID, reason string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	return g.withLock(incarnation, func() error {
		path := g.layout.HolderPath(incarnation)

		existing, err := readHolder(path)
		if err != nil {
			return err
		}

		if existing == nil {
			return nil
		}

		now := g.now().UTC()
		existing.State = HolderBlocked
		existing.BlockedReason = reason
		existing.BlockedAt = &now

		return writeHolder(path, existing)
	})
}

// Inspect returns the record, or nil when the incarnation is free.
func (g *Gate) Inspect(incarnation ID) (*Holder, error) {
	return readHolder(g.layout.HolderPath(incarnation))
}

// List returns every holder record on this node.
func (g *Gate) List() ([]Holder, error) {
	entries, err := os.ReadDir(g.layout.HoldersDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, fmt.Errorf("blockvol: list holders: %w", err)
	}

	var holders []Holder
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, holderSuffix) {
			continue
		}

		h, err := readHolder(filepath.Join(g.layout.HoldersDir(), name))
		if err != nil {
			return nil, err
		}

		if h != nil {
			holders = append(holders, *h)
		}
	}

	return holders, nil
}
