package blockvol

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// HolderState is the admission state recorded for an incarnation.
type HolderState string

const (
	// HolderActive means a Firecracker process is believed to own the image.
	HolderActive HolderState = "active"
	// HolderBlocked means the recorded process is gone but nobody has yet
	// reclaimed the incarnation with evidence; new binds are refused.
	HolderBlocked HolderState = "blocked"
)

// Holder is the fact of record for "who may write this incarnation". It is
// persisted as JSON under holders/<incarnation>.json and deliberately kept
// outside the subvolumes so snapshots never copy it.
//
// Liveness of the holder is established by (node_boot_id, fc_pid,
// fc_starttime): a pid alone is recycled, but a pid whose /proc starttime
// matches is the same process; a different boot id means every process of the
// previous boot is gone.
type Holder struct {
	Incarnation   ID          `json:"incarnation"`
	SandboxID     string      `json:"sandbox_id"`
	ExecutionID   string      `json:"execution_id"`
	NodeBootID    string      `json:"node_boot_id"`
	FCPid         int         `json:"fc_pid"`
	FCStarttime   uint64      `json:"fc_starttime"`
	BackingPath   string      `json:"backing_path"`
	CreatedAt     time.Time   `json:"created_at"`
	State         HolderState `json:"state"`
	BlockedReason string      `json:"blocked_reason,omitempty"`
	BlockedAt     *time.Time  `json:"blocked_at,omitempty"`
}

// ProcessLiveness answers whether a recorded process identity still exists.
// It is an interface so the gate state machine can be tested without
// spawning processes.
type ProcessLiveness interface {
	// Starttime returns the process start time (clock ticks since boot, field
	// 22 of /proc/<pid>/stat). It fails if the process does not exist.
	Starttime(pid int) (uint64, error)
	// Alive reports whether a process with this pid exists and has the given
	// starttime, i.e. it is the very process that was recorded.
	Alive(pid int, starttime uint64) (bool, error)
}

// ProcLiveness reads process identity from procfs.
type ProcLiveness struct {
	// ProcDir defaults to /proc.
	ProcDir string
}

func (p ProcLiveness) procDir() string {
	if p.ProcDir == "" {
		return "/proc"
	}

	return p.ProcDir
}

func (p ProcLiveness) Starttime(pid int) (uint64, error) {
	raw, err := os.ReadFile(filepath.Join(p.procDir(), strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, fmt.Errorf("blockvol: read stat of pid %d: %w", pid, err)
	}

	return parseStarttime(raw)
}

func (p ProcLiveness) Alive(pid int, starttime uint64) (bool, error) {
	current, err := p.Starttime(pid)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}

		return false, err
	}

	return current == starttime, nil
}

// parseStarttime extracts field 22 of /proc/<pid>/stat. The comm field is
// wrapped in parentheses and may contain spaces, so fields are counted after
// the last ')'.
func parseStarttime(stat []byte) (uint64, error) {
	s := string(stat)
	end := strings.LastIndexByte(s, ')')
	if end < 0 {
		return 0, errors.New("blockvol: malformed /proc/<pid>/stat: no comm field")
	}

	fields := strings.Fields(s[end+1:])
	// fields[0] is state (field 3); starttime is field 22 -> index 19.
	const starttimeIndex = 22 - 3
	if len(fields) <= starttimeIndex {
		return 0, errors.New("blockvol: malformed /proc/<pid>/stat: too few fields")
	}

	v, err := strconv.ParseUint(fields[starttimeIndex], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("blockvol: parse starttime: %w", err)
	}

	return v, nil
}

// NodeBootID returns the kernel boot id, which changes on every reboot.
func NodeBootID() (string, error) {
	return readBootID("/proc/sys/kernel/random/boot_id")
}

func readBootID(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("blockvol: read boot id: %w", err)
	}

	id := strings.TrimSpace(string(raw))
	if _, err := ParseID(id); err != nil {
		return "", err
	}

	return id, nil
}

// readHolder returns (nil, nil) when no record exists. A record that exists
// but cannot be decoded is reported as an error: the conservative reading of
// a torn write is "somebody may hold this", never "free".
func readHolder(path string) (*Holder, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil //nolint:nilnil // absence is the meaningful answer here
		}

		return nil, fmt.Errorf("blockvol: read holder %s: %w", path, err)
	}

	var h Holder
	if err := json.Unmarshal(raw, &h); err != nil {
		return nil, fmt.Errorf("blockvol: corrupt holder record %s: %w", path, err)
	}

	return &h, nil
}

// writeHolder publishes the record atomically: write a sibling temp file,
// fsync, rename over the record, fsync the directory.
func writeHolder(path string, h *Holder) error {
	raw, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return fmt.Errorf("blockvol: encode holder: %w", err)
	}

	tmp := path + ".tmp"

	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("blockvol: create %s: %w", tmp, err)
	}

	if _, err := f.Write(raw); err != nil {
		f.Close()

		return fmt.Errorf("blockvol: write %s: %w", tmp, err)
	}

	if err := f.Sync(); err != nil {
		f.Close()

		return fmt.Errorf("blockvol: fsync %s: %w", tmp, err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("blockvol: close %s: %w", tmp, err)
	}

	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("blockvol: rename %s: %w", tmp, err)
	}

	return fsyncPath(filepath.Dir(path))
}

func removeHolder(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("blockvol: remove holder %s: %w", path, err)
	}

	return fsyncPath(filepath.Dir(path))
}
