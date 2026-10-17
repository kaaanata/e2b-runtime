//go:build linux

package volumes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// Shared-volume host layout on Btrfs.
//
// With VOLUME_BTRFS_SUBVOLUMES set, a volume directory is a Btrfs subvolume
// rather than a plain directory. That buys two things a directory cannot give:
// a host-side read-only snapshot of one volume (a backup or version point; with
// several writers it is crash-consistent, nothing more), and a qgroup limit per
// volume. Everything the NFS proxy does with the directory is unchanged — a
// subvolume is a directory to every path-based API.
//
// The flag is opt-in and default off because the persistent-volume root has to
// be on Btrfs for it to work; an operator on ext4/xfs keeps today's layout.

const (
	// btrfsSuperMagic is the f_type statfs(2) reports for a Btrfs filesystem.
	btrfsSuperMagic = 0x9123683E
	// btrfsFirstFreeObjectID is the inode number every Btrfs subvolume root has:
	// that constant inode is how a subvolume is told from a directory without
	// running the btrfs tool.
	btrfsFirstFreeObjectID = 256

	btrfsCommandTimeout = 30 * time.Second
)

var ErrNotBtrfs = errors.New("volume root is not on a Btrfs filesystem")

// btrfsRunner runs the btrfs tool. A variable so tests can observe or replace
// the invocations.
var btrfsRunner = func(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, btrfsCommandTimeout)
	defer cancel()

	return exec.CommandContext(ctx, "btrfs", args...).CombinedOutput()
}

// isBtrfsSubvolume reports whether path is the root of a Btrfs subvolume: it
// lives on Btrfs and carries the subvolume root inode number. A missing path is
// not an error here; it is simply not a subvolume.
func isBtrfsSubvolume(path string) (bool, error) {
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return false, nil
		}

		return false, fmt.Errorf("lstat %s: %w", path, err)
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFDIR || st.Ino != btrfsFirstFreeObjectID {
		return false, nil
	}

	onBtrfs, err := isOnBtrfs(path)
	if err != nil {
		return false, err
	}

	return onBtrfs, nil
}

func isOnBtrfs(path string) (bool, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return false, fmt.Errorf("statfs %s: %w", path, err)
	}

	return uint32(fs.Type) == btrfsSuperMagic, nil
}

// createBtrfsVolume makes fullPath a Btrfs subvolume (creating the parent
// directories as plain directories) and, when limitBytes > 0, caps it with a
// qgroup limit. Idempotent: an existing subvolume is kept and only the limit is
// re-applied. The parent must be on Btrfs; otherwise ErrNotBtrfs, so a
// misconfigured flag fails loudly instead of silently making directories.
//
// The qgroup limit needs quotas enabled on the filesystem (`btrfs quota enable`),
// which is the operator's decision for the whole filesystem and not done here.
func createBtrfsVolume(ctx context.Context, fullPath string, limitBytes int64) error {
	parent := filepath.Dir(fullPath)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("failed to create volume parent %s: %w", parent, err)
	}

	onBtrfs, err := isOnBtrfs(parent)
	if err != nil {
		return err
	}
	if !onBtrfs {
		return fmt.Errorf("%w: %s", ErrNotBtrfs, parent)
	}

	exists, err := isBtrfsSubvolume(fullPath)
	if err != nil {
		return err
	}
	if !exists {
		if _, statErr := os.Lstat(fullPath); statErr == nil {
			return fmt.Errorf("volume path %s exists but is not a subvolume", fullPath)
		}
		if out, err := btrfsRunner(ctx, "subvolume", "create", fullPath); err != nil {
			return fmt.Errorf("btrfs subvolume create %s: %w\n%s", fullPath, err, out)
		}
		if err := os.Chmod(fullPath, 0o700); err != nil {
			return fmt.Errorf("chmod %s: %w", fullPath, err)
		}
	}

	if limitBytes > 0 {
		if out, err := btrfsRunner(ctx, "qgroup", "limit", strconv.FormatInt(limitBytes, 10), fullPath); err != nil {
			return fmt.Errorf("btrfs qgroup limit %d %s: %w\n%s", limitBytes, fullPath, err, out)
		}
	}

	return nil
}

// deleteVolumePath removes a volume directory. A Btrfs subvolume cannot be
// rmdir'd and RemoveAll would walk and unlink every file one by one, so a
// subvolume is deleted as one; anything else keeps the RemoveAll behaviour. A
// path that does not exist is not an error, matching RemoveAll.
func deleteVolumePath(ctx context.Context, fullPath string) error {
	subvolume, err := isBtrfsSubvolume(fullPath)
	if err != nil {
		return err
	}
	if !subvolume {
		return os.RemoveAll(fullPath)
	}

	// --commit-after makes the delete durable before returning, so a volume the
	// control plane already forgot cannot reappear after a crash.
	if out, err := btrfsRunner(ctx, "subvolume", "delete", "--commit-after", fullPath); err != nil {
		return fmt.Errorf("btrfs subvolume delete %s: %w\n%s", fullPath, err, out)
	}

	return nil
}
