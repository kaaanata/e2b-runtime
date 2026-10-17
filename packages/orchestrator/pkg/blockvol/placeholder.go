package blockvol

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// Placeholder returns the path of the shared, unformatted, sparse image for
// one capacity tier. Templates boot with the placeholder in the reserved
// workspace drive slot so that the Firecracker device geometry is fixed at
// build time; restore swaps the real incarnation image into the same path
// slot. Because it is sparse it costs no Btrfs data until written, and it is
// never written: the guest sees an unformatted device and envd refuses to
// mount it.
//
// The image is created once per tier under placeholders/<bytes>.img through
// a temp file and rename, so concurrent callers converge on one inode.
func (s *Service) Placeholder(ctx context.Context, capacityBytes uint64) (string, error) {
	if capacityBytes == 0 {
		return "", errors.New("blockvol: placeholder capacity must be positive")
	}

	if err := ctx.Err(); err != nil {
		return "", err
	}

	path := s.layout.PlaceholderImage(capacityBytes)

	if st, err := os.Stat(path); err == nil {
		if st.Size() != int64(capacityBytes) { //nolint:gosec // capacity is a bounded tier value
			return "", fmt.Errorf("blockvol: placeholder %s has size %d, want %d", path, st.Size(), capacityBytes)
		}

		return path, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("blockvol: stat placeholder: %w", err)
	}

	if err := os.MkdirAll(s.layout.PlaceholdersDir(), 0o700); err != nil {
		return "", fmt.Errorf("blockvol: create placeholders dir: %w", err)
	}

	tmp, err := os.CreateTemp(s.layout.PlaceholdersDir(), fmt.Sprintf(".%d-*.tmp", capacityBytes))
	if err != nil {
		return "", fmt.Errorf("blockvol: create placeholder temp: %w", err)
	}

	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	if err := tmp.Truncate(int64(capacityBytes)); err != nil { //nolint:gosec // capacity is a bounded tier value
		tmp.Close()
		cleanup()

		return "", fmt.Errorf("blockvol: size placeholder: %w", err)
	}

	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()

		return "", fmt.Errorf("blockvol: fsync placeholder: %w", err)
	}

	if err := tmp.Close(); err != nil {
		cleanup()

		return "", fmt.Errorf("blockvol: close placeholder: %w", err)
	}

	if err := os.Chmod(tmpPath, 0o600); err != nil {
		cleanup()

		return "", fmt.Errorf("blockvol: chmod placeholder: %w", err)
	}

	// If another caller published first, rename still succeeds and replaces
	// the file with an identical sparse image; readers holding the old inode
	// are unaffected because its size is the same.
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()

		return "", fmt.Errorf("blockvol: publish placeholder: %w", err)
	}

	if err := fsyncPath(s.layout.PlaceholdersDir()); err != nil {
		return "", err
	}

	return path, nil
}
