package blockvol

import (
	"context"
	"errors"
	"fmt"
)

// ErrShrinkRefused is returned when a grow names a size below the image's
// current size. Workspace volumes only ever grow: a guest that has the
// filesystem mounted (or a revision that recorded the size) would read past
// the end of a shrunk device.
var ErrShrinkRefused = errors.New("blockvol: workspace images never shrink")

// DescribeIncarnation reads the on-disk facts of a writable image (subvolume
// UUID, ext4 UUID, size). It only reads.
func (s *Service) DescribeIncarnation(ctx context.Context, inc IncarnationRef) (IncarnationInfo, error) {
	if err := inc.validate(); err != nil {
		return IncarnationInfo{}, err
	}

	ok, err := exists(s.layout.IncarnationImage(inc))
	if err != nil {
		return IncarnationInfo{}, err
	}

	if !ok {
		return IncarnationInfo{}, fmt.Errorf("%w: %s", ErrIncarnationNotFound, inc.Incarnation)
	}

	return s.incarnationInfo(ctx, inc, ID{})
}

// IncarnationImagePath is the host path of an incarnation's ext4 image.
func (s *Service) IncarnationImagePath(inc IncarnationRef) (string, error) {
	if err := inc.validate(); err != nil {
		return "", err
	}

	return s.layout.IncarnationImage(inc), nil
}

// GrowImage extends a bound incarnation's backing image to sizeBytes without
// touching the filesystem inside it: the device a paused guest sees grows, and
// the guest grows the mounted ext4 online afterwards (envd /volumes/resize).
// This is the paired-memory resize path, where an offline resize2fs would
// rewrite a superblock the restored guest kernel still caches.
//
// executionID must be the recorded holder: the caller grows the image it was
// admitted to write, between Bind and the snapshot load. Equal size is a
// no-op, a smaller size is ErrShrinkRefused. Extension goes through fallocate
// so the reservation contract of Create (no host ENOSPC mid-flight) holds for
// the new range too; existing extents, shared with revisions, are untouched.
func (s *Service) GrowImage(ctx context.Context, inc IncarnationRef, executionID string, sizeBytes uint64) error {
	if err := inc.validate(); err != nil {
		return err
	}

	if executionID == "" {
		return fmt.Errorf("%w: online grow needs the holder's execution id", ErrHolderMismatch)
	}

	holder, err := s.gate.Inspect(inc.Incarnation)
	if err != nil {
		return err
	}

	if holder == nil || holder.ExecutionID != executionID {
		return fmt.Errorf("%w: grow by %s", ErrHolderMismatch, executionID)
	}

	return s.growImage(ctx, inc, sizeBytes)
}

// GrowFilesystemOffline extends an unbound incarnation's image to sizeBytes
// and grows the ext4 inside it with resize2fs before any VM opens it. It is
// the cold-boot resize path (filesystem-quiesced revisions): there is no
// guest kernel state to agree with, so the host may rewrite the filesystem.
//
// It refuses while any holder record exists, exactly like an offline Seal:
// resize2fs on an image a VM may still write would corrupt it. A dirty
// journal (a frozen-but-mounted image) is replayed with e2fsck first, the same
// recovery the guest kernel would do on mount.
func (s *Service) GrowFilesystemOffline(ctx context.Context, inc IncarnationRef, sizeBytes uint64) error {
	if err := inc.validate(); err != nil {
		return err
	}

	holder, err := s.gate.Inspect(inc.Incarnation)
	if err != nil {
		return err
	}

	if holder != nil {
		return fmt.Errorf("%w: offline resize, but record held by %s (%s)", ErrHolderMismatch, holder.ExecutionID, holder.State)
	}

	if err := s.growImage(ctx, inc, sizeBytes); err != nil {
		return err
	}

	image := s.layout.IncarnationImage(inc)

	if _, fsckErr := s.tools.run.Run(ctx, "e2fsck", "-f", "-p", image); fsckErr != nil {
		var cmdErr *CommandError
		// 1 = errors corrected, 2 = corrected and reboot advised; both leave a
		// consistent filesystem.
		if !errors.As(fsckErr, &cmdErr) || cmdErr.ExitCode > 2 {
			return fmt.Errorf("blockvol: e2fsck before offline resize: %w", fsckErr)
		}
	}

	if _, err := s.tools.run.Run(ctx, "resize2fs", image); err != nil {
		return err
	}

	return fsyncPath(image)
}

func (s *Service) growImage(ctx context.Context, inc IncarnationRef, sizeBytes uint64) error {
	image := s.layout.IncarnationImage(inc)

	ok, err := exists(image)
	if err != nil {
		return err
	}

	if !ok {
		return fmt.Errorf("%w: %s", ErrIncarnationNotFound, inc.Incarnation)
	}

	current, _, err := imageSize(image)
	if err != nil {
		return err
	}

	switch {
	case sizeBytes < current:
		return fmt.Errorf("%w: %s is %d bytes, asked for %d", ErrShrinkRefused, inc.Incarnation, current, sizeBytes)
	case sizeBytes == current:
		return nil
	}

	if err := s.checkCapacity(ctx, sizeBytes-current); err != nil {
		return err
	}

	if err := s.tools.fallocate(ctx, image, sizeBytes); err != nil {
		return err
	}

	return fsyncPath(image)
}
