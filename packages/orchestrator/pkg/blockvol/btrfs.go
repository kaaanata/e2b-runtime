package blockvol

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// Runner executes an external tool and returns its stdout. Implementations
// must return a *CommandError when the process exits non-zero so that callers
// can inspect stderr.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// CommandError carries the stderr of a failed tool invocation.
type CommandError struct {
	Name     string
	Args     []string
	ExitCode int
	Stderr   string
	Err      error
}

func (e *CommandError) Error() string {
	return fmt.Sprintf("blockvol: %s %s: exit %d: %s", e.Name, strings.Join(e.Args, " "), e.ExitCode, strings.TrimSpace(e.Stderr))
}

func (e *CommandError) Unwrap() error { return e.Err }

// ExecRunner runs tools through os/exec. The sbin directories are appended to
// PATH because the orchestrator may run with a user PATH while mkfs/tune2fs
// live in /usr/sbin.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "PATH="+os.Getenv("PATH")+":/usr/sbin:/sbin:/usr/local/sbin")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		code := -1
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		}

		return stdout.Bytes(), &CommandError{Name: name, Args: args, ExitCode: code, Stderr: stderr.String(), Err: err}
	}

	return stdout.Bytes(), nil
}

// tools wraps the CLI programs the backend depends on. No cgo, no ioctls: the
// orchestrator already ships with btrfs-progs and e2fsprogs on the worker.
type tools struct {
	run Runner
}

// SubvolumeInfo is the subset of `btrfs subvolume show` the backend records.
type SubvolumeInfo struct {
	UUID       string
	ParentUUID string
	ID         string
	Generation string
	ReadOnly   bool
	Snapshots  []string
}

func (t tools) subvolumeCreate(ctx context.Context, path string) error {
	_, err := t.run.Run(ctx, "btrfs", "subvolume", "create", path)

	return err
}

func (t tools) subvolumeSnapshot(ctx context.Context, src, dst string, readOnly bool) error {
	args := []string{"subvolume", "snapshot"}
	if readOnly {
		args = append(args, "-r")
	}

	args = append(args, src, dst)
	_, err := t.run.Run(ctx, "btrfs", args...)

	return err
}

// subvolumeDelete is idempotent: a missing path is not an error.
func (t tools) subvolumeDelete(ctx context.Context, path string) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}

	_, err := t.run.Run(ctx, "btrfs", "subvolume", "delete", path)

	return err
}

func (t tools) subvolumeShow(ctx context.Context, path string) (SubvolumeInfo, error) {
	out, err := t.run.Run(ctx, "btrfs", "subvolume", "show", path)
	if err != nil {
		return SubvolumeInfo{}, err
	}

	return parseSubvolumeShow(out)
}

// parseSubvolumeShow reads the tab-separated "Key: value" block printed by
// `btrfs subvolume show`. The first line is the path and is skipped; the
// "Snapshot(s):" section lists one path per following indented line.
func parseSubvolumeShow(out []byte) (SubvolumeInfo, error) {
	var info SubvolumeInfo
	lines := strings.Split(string(out), "\n")
	inSnapshots := false

	for i, line := range lines {
		if i == 0 {
			continue
		}

		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		if inSnapshots {
			if !strings.Contains(trimmed, ":") {
				info.Snapshots = append(info.Snapshots, trimmed)

				continue
			}

			inSnapshots = false
		}

		key, value, found := strings.Cut(trimmed, ":")
		if !found {
			continue
		}

		value = strings.TrimSpace(value)

		switch strings.TrimSpace(key) {
		case "UUID":
			info.UUID = value
		case "Parent UUID":
			if value != "-" {
				info.ParentUUID = value
			}
		case "Subvolume ID":
			info.ID = value
		case "Generation":
			info.Generation = value
		case "Flags":
			info.ReadOnly = strings.Contains(value, "readonly")
		case "Snapshot(s)":
			inSnapshots = true
		}
	}

	if info.UUID == "" {
		return SubvolumeInfo{}, errors.New("blockvol: `btrfs subvolume show` output has no UUID line")
	}

	return info, nil
}

func (t tools) filesystemUsage(ctx context.Context, path string) ([]byte, error) {
	return t.run.Run(ctx, "btrfs", "filesystem", "usage", "-b", path)
}

// fallocate reserves the full capacity so a writer can never hit host ENOSPC
// mid-flight; -E nodiscard in mkfs keeps the reservation.
func (t tools) fallocate(ctx context.Context, path string, bytes uint64) error {
	_, err := t.run.Run(ctx, "fallocate", "-l", fmt.Sprintf("%d", bytes), path)

	return err
}

// mkfsExt4 formats the image with a caller-chosen UUID.
//
//   - -b 4096 pins the block size: without it mke2fs picks 1 KiB blocks for
//     images under 512 MiB ("small" type), and a later grow of such a volume
//     would carry that geometry forever.
//   - metadata_csum_seed stores the checksum seed in the superblock, so a
//     later `tune2fs -U` (Clone) only rewrites the superblock instead of every
//     metadata checksum. The pinned 6.1 guest kernel supports it.
//   - -E nodiscard keeps the fallocate reservation.
func (t tools) mkfsExt4(ctx context.Context, image, fsUUID string) error {
	_, err := t.run.Run(ctx, "mkfs.ext4", "-q", "-F", "-b", "4096",
		"-O", "metadata_csum,metadata_csum_seed", "-E", "nodiscard", "-U", fsUUID, image)

	return err
}

// tune2fsRandomUUID gives a cloned image a new filesystem UUID so that two
// volumes can be mounted in the same guest. tune2fs refuses when the image
// still carries a dirty journal (a quiesced snapshot of a mounted fs); in that
// case the journal is replayed with e2fsck -p, which is exactly what the guest
// kernel would do on mount, and the UUID change is retried.
func (t tools) tune2fsRandomUUID(ctx context.Context, image string) error {
	_, err := t.run.Run(ctx, "tune2fs", "-U", "random", image)
	if err == nil {
		return nil
	}

	var cmdErr *CommandError
	if !errors.As(err, &cmdErr) {
		return err
	}

	if _, fsckErr := t.run.Run(ctx, "e2fsck", "-f", "-p", image); fsckErr != nil {
		var fsckCmdErr *CommandError
		// e2fsck exit 1 = errors corrected, 2 = corrected and reboot advised;
		// both leave a consistent filesystem behind.
		if !errors.As(fsckErr, &fsckCmdErr) || fsckCmdErr.ExitCode > 2 {
			return fmt.Errorf("tune2fs failed (%w) and e2fsck did not recover the image: %w", err, fsckErr)
		}
	}

	_, err = t.run.Run(ctx, "tune2fs", "-U", "random", image)

	return err
}

// ext4UUID reads the filesystem UUID from the superblock with blkid; it works
// on regular files and does not need the image to be attached to a device.
func (t tools) ext4UUID(ctx context.Context, image string) (string, error) {
	out, err := t.run.Run(ctx, "blkid", "-p", "-o", "value", "-s", "UUID", image)
	if err != nil {
		return "", err
	}

	id := strings.TrimSpace(string(out))
	if _, parseErr := ParseID(id); parseErr != nil {
		return "", fmt.Errorf("blockvol: blkid returned %q for %s: %w", id, image, parseErr)
	}

	return id, nil
}

// imageSize returns the apparent size and the bytes actually allocated.
func imageSize(path string) (size uint64, allocated uint64, err error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, 0, fmt.Errorf("blockvol: stat %s: %w", path, err)
	}

	return uint64(st.Size), uint64(st.Blocks) * 512, nil //nolint:gosec // st_size is never negative for a regular file
}

// fsyncPath flushes a file or directory so a published rename survives a crash.
func fsyncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("blockvol: open for fsync %s: %w", path, err)
	}
	defer f.Close()

	if err := f.Sync(); err != nil {
		return fmt.Errorf("blockvol: fsync %s: %w", path, err)
	}

	return nil
}

// publish moves a staged artefact into place and durably records the move in
// the destination's parent directory.
func publish(staged, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("blockvol: create parent of %s: %w", dst, err)
	}

	if err := os.Rename(staged, dst); err != nil {
		return fmt.Errorf("blockvol: publish %s -> %s: %w", staged, dst, err)
	}

	return fsyncPath(filepath.Dir(dst))
}

// exists reports whether path exists (any type).
func exists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}

	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}

	return false, fmt.Errorf("blockvol: stat %s: %w", path, err)
}
