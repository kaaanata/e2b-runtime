package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/sync/errgroup"

	"github.com/e2b-dev/infra/packages/envd/internal/logs"
)

// mountRecord is one entry of the volume mount ledger (API.mountedPaths): what
// /init mounted at a path, for which lifecycle, and (for block volumes) from which
// guest device. Everything that later acts on a mount — the lifecycle remount
// decision, the mount-set freeze, /volumes/resize — reads this ledger and never a
// path supplied by the caller.
type mountRecord struct {
	LifecycleID string
	SourceType  VolumeMountSourceType
	// Device is the guest block device (e.g. /dev/vdb) a block volume was mounted
	// from. Empty for NFS.
	Device string
}

// nfsDefaultOptions is the option set used when /init carries no mount_options:
// the behaviour every orchestrator predating per-mount options gets, unchanged.
var nfsDefaultOptions = strings.Join([]string{
	// wait for data to be sent to proxy server before returning.
	// async might cause issues if the sandbox is shut down suddenly.
	"sync",

	"rsize=1048576",  // 1 MB read buffer
	"wsize=1048576",  // 1 MB write buffer
	"mountproto=tcp", // nfs proxy only supports tcp
	"mountport=2049", // nfs proxy only supports mounting on port 2049
	"proto=tcp",      // nfs proxy only supports tcp
	"port=2049",      // nfs proxy only supports mounting on port 2049
	"nfsvers=3",      // nfs proxy is nfs version 3
	"noacl",          // no reason for acl in the sandbox

	// disable caching so that pause/resume works correctly
	"noac",
	"lookupcache=none",
}, ",")

// nfsOptionWhitelist is what an orchestrator may put in VolumeMount.mount_options.
// The value is the policy for the option's argument: exactly one permitted value,
// or any token (validated by nfsOptionValuePattern), or no argument at all.
//
// The set is deliberately the knobs that change caching/consistency and the
// transport facts of the NFS proxy. Anything that could point the mount at
// another server, change the security flavour or run a helper is absent.
var nfsOptionWhitelist = map[string]nfsOptionPolicy{
	"sync":        nfsOptionFlag,
	"async":       nfsOptionFlag,
	"rsize":       nfsOptionAnyValue,
	"wsize":       nfsOptionAnyValue,
	"proto":       {exact: "tcp"},
	"mountproto":  {exact: "tcp"},
	"port":        nfsOptionAnyValue,
	"mountport":   nfsOptionAnyValue,
	"nfsvers":     {exact: "3"},
	"noacl":       nfsOptionFlag,
	"noac":        nfsOptionFlag,
	"ac":          nfsOptionFlag,
	"lookupcache": nfsOptionAnyValue,
	"actimeo":     nfsOptionAnyValue,
	"acregmin":    nfsOptionAnyValue,
	"acregmax":    nfsOptionAnyValue,
	"acdirmin":    nfsOptionAnyValue,
	"acdirmax":    nfsOptionAnyValue,
	"retry":       nfsOptionAnyValue,
	"fg":          nfsOptionFlag,
	"hard":        nfsOptionFlag,
	"nolock":      nfsOptionFlag,
	"local_lock":  nfsOptionAnyValue,
}

type nfsOptionPolicy struct {
	// exact, when set, is the only value accepted for the option.
	exact string
	// anyValue accepts any value matching nfsOptionValuePattern.
	anyValue bool
	// flag means the option takes no value.
	flag bool
}

var (
	nfsOptionFlag     = nfsOptionPolicy{flag: true}
	nfsOptionAnyValue = nfsOptionPolicy{anyValue: true}
)

// nfsOptionValuePattern bounds option values to a single mount(8) token: no
// commas (which would smuggle a second option), no whitespace, no shell
// metacharacters. Values are handed to exec directly, so this is defence in
// depth against an option list that mount(8) would parse differently than we did.
var nfsOptionValuePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

var ErrInvalidNFSMountOption = errors.New("invalid NFS mount option")

// nfsMountOptions returns the option string for mount(8). nil or empty requested
// options select the built-in default set verbatim. Otherwise every entry is
// checked against nfsOptionWhitelist and `nolock` is appended unless the caller
// chose a locking mode itself (`nolock` or `local_lock=`): the NFS proxy has no
// lock manager, so a mount without it hangs on the first fcntl lock.
func nfsMountOptions(requested *[]string) (string, error) {
	if requested == nil || len(*requested) == 0 {
		return nfsDefaultOptions, nil
	}

	out := make([]string, 0, len(*requested)+1)
	lockingChosen := false

	for _, raw := range *requested {
		opt := strings.TrimSpace(raw)
		if opt == "" {
			return "", fmt.Errorf("%w: empty entry", ErrInvalidNFSMountOption)
		}

		key, value, hasValue := strings.Cut(opt, "=")

		policy, ok := nfsOptionWhitelist[key]
		if !ok {
			return "", fmt.Errorf("%w: %q is not in the whitelist", ErrInvalidNFSMountOption, opt)
		}

		switch {
		case policy.flag:
			if hasValue {
				return "", fmt.Errorf("%w: %q takes no value", ErrInvalidNFSMountOption, opt)
			}
		case policy.exact != "":
			if !hasValue || value != policy.exact {
				return "", fmt.Errorf("%w: %q must be %s=%s", ErrInvalidNFSMountOption, opt, key, policy.exact)
			}
		case policy.anyValue:
			if !hasValue || !nfsOptionValuePattern.MatchString(value) {
				return "", fmt.Errorf("%w: %q needs a value matching %s", ErrInvalidNFSMountOption, opt, nfsOptionValuePattern)
			}
		}

		if key == "nolock" || key == "local_lock" {
			lockingChosen = true
		}

		out = append(out, opt)
	}

	if !lockingChosen {
		out = append(out, "nolock")
	}

	return strings.Join(out, ","), nil
}

const (
	nfsMountTimeout = 10 * time.Second
	// blockMountTimeout bounds the whole block-volume mount phase of one /init:
	// resolving the device, size check and mount(8). The device is local, so this
	// is generous.
	blockMountTimeout = 10 * time.Second
	resizeTimeout     = 5 * time.Minute
)

var (
	ErrVolumeMountInvalid  = errors.New("invalid volume mount")
	ErrBlockDeviceNotFound = errors.New("block device not found")
	ErrBlockDeviceTooSmall = errors.New("block device smaller than the declared capacity")
	ErrNotABlockVolume     = errors.New("path is not a block volume envd mounted")
)

// fsUUIDPattern is the canonical ext4 UUID form. The UUID becomes a path under
// /dev/disk/by-uuid and an argument to blkid, so anything else is refused.
var fsUUIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// sourceType returns the discriminator, treating an absent one as nfs: that is
// the only shape an orchestrator predating block volumes could send.
func (v VolumeMount) sourceType() VolumeMountSourceType {
	if v.SourceType == nil || *v.SourceType == "" {
		return Nfs
	}

	return *v.SourceType
}

// validate checks the per-variant required fields the schema cannot express
// (nfs_target is required only for nfs, fs_uuid only for block).
func (v VolumeMount) validate() error {
	if v.Path == "" || !filepath.IsAbs(v.Path) {
		return fmt.Errorf("%w: path %q must be absolute", ErrVolumeMountInvalid, v.Path)
	}
	if filepath.Clean(v.Path) == rootfsMountpoint {
		// Mounting over / would hide the rootfs and make the mount set freeze
		// the same mountpoint twice.
		return fmt.Errorf("%w: path %q is the root filesystem", ErrVolumeMountInvalid, v.Path)
	}

	switch v.sourceType() {
	case Nfs:
		if derefString(v.NfsTarget) == "" {
			return fmt.Errorf("%w: nfs mount at %q has no nfs_target", ErrVolumeMountInvalid, v.Path)
		}
	case Block:
		uuid := derefString(v.FsUuid)
		if !fsUUIDPattern.MatchString(uuid) {
			return fmt.Errorf("%w: block mount at %q has an invalid fs_uuid %q", ErrVolumeMountInvalid, v.Path, uuid)
		}
		if v.MountOptions != nil && len(*v.MountOptions) > 0 {
			return fmt.Errorf("%w: block mount at %q carries mount_options, which are NFS only", ErrVolumeMountInvalid, v.Path)
		}
	default:
		return fmt.Errorf("%w: unknown source_type %q at %q", ErrVolumeMountInvalid, *v.SourceType, v.Path)
	}

	return nil
}

// setupVolumes mounts (or re-mounts, on a lifecycle change) every volume in the
// /init request. Validation of the whole list happens before any mount, so a
// malformed entry fails the request without leaving half the set mounted.
//
// Paths are normalized with filepath.Clean before they enter the ledger, so the
// mount-set freeze and /volumes/resize look up the same key /init stored. Mounts
// run concurrently within one path depth and depth by depth from the shallowest,
// so a volume nested under another (an NFS share inside a block workspace, say)
// is mounted after its parent instead of racing it and ending up underneath it.
func (a *API) setupVolumes(ctx context.Context, logger zerolog.Logger, lifecycleID *string, mounts []VolumeMount) (e error) {
	normalized := make([]VolumeMount, 0, len(mounts))
	seen := make(map[string]struct{}, len(mounts))

	for _, volume := range mounts {
		if err := volume.validate(); err != nil {
			return err
		}

		volume.Path = filepath.Clean(volume.Path)
		if _, dup := seen[volume.Path]; dup {
			return fmt.Errorf("%w: path %q is listed twice", ErrVolumeMountInvalid, volume.Path)
		}
		seen[volume.Path] = struct{}{}
		normalized = append(normalized, volume)
	}

	// Prevent concurrent mounting attempts
	if !a.isMountingNFS.CompareAndSwap(false, true) {
		logger.Debug().Msg("Volumes already mounting")

		return e
	}
	defer a.isMountingNFS.Store(false)

	logger.Debug().Msg("Setting up volumes")

	ctx = context.WithoutCancel(ctx)                                                 // don't allow request context cancellation to propagate
	ctx, cancel := context.WithTimeout(ctx, max(nfsMountTimeout, blockMountTimeout)) // don't let the mounts run forever
	defer cancel()

	requestLifecycleID := derefString(lifecycleID)

	var pending []VolumeMount
	for _, volume := range normalized {
		// Check if this path is already mounted for the current lifecycle
		record, isMounted := a.loadMount(volume.Path)
		if !shouldRemountNFS(isMounted, record.LifecycleID, requestLifecycleID) {
			logger.Debug().Msgf("Skipping %q, already mounted for lifecycle %q", volume.Path, requestLifecycleID)

			continue
		}

		if isMounted {
			logger.Debug().Msgf("Lifecycle changed for %q: %q -> %q", volume.Path, record.LifecycleID, requestLifecycleID)
		}

		pending = append(pending, volume)
	}

	for _, level := range mountLevels(pending) {
		wg, wgCtx := errgroup.WithContext(ctx)

		for _, volume := range level {
			wg.Go(func() error {
				// Unmount if currently mounted (handles stale mounts from previous lifecycle)
				if err := a.unmountStale(wgCtx, logger, volume.Path); err != nil {
					return fmt.Errorf("failed to unmount stale mount at %q: %w", volume.Path, err)
				}

				switch volume.sourceType() {
				case Block:
					return a.setupBlock(wgCtx, logger, requestLifecycleID, volume)
				default:
					return a.setupNFS(wgCtx, logger, requestLifecycleID, volume)
				}
			})
		}

		if err := wg.Wait(); err != nil {
			return err
		}
	}

	return nil
}

// mountLevels groups mounts by path depth, shallowest first. Mounts of one depth
// cannot be nested in each other, so each group can be mounted concurrently.
func mountLevels(mounts []VolumeMount) [][]VolumeMount {
	byDepth := make(map[int][]VolumeMount)
	for _, m := range mounts {
		d := pathDepth(m.Path)
		byDepth[d] = append(byDepth[d], m)
	}

	depths := slices.Sorted(maps.Keys(byDepth))
	levels := make([][]VolumeMount, 0, len(depths))
	for _, d := range depths {
		levels = append(levels, byDepth[d])
	}

	return levels
}

// pathDepth is the number of components of a cleaned absolute path ("/" is 0).
func pathDepth(path string) int {
	if path == "/" {
		return 0
	}

	return strings.Count(path, "/")
}

// setupNFS mounts one NFS volume and records it in the ledger.
func (a *API) setupNFS(ctx context.Context, logger zerolog.Logger, lifecycleID string, volume VolumeMount) error {
	options, err := nfsMountOptions(volume.MountOptions)
	if err != nil {
		return fmt.Errorf("failed to build NFS mount options for %q: %w", volume.Path, err)
	}

	target := derefString(volume.NfsTarget)
	logger.Debug().Msgf("Setting up NFS %s at %q", target, volume.Path)

	if err := a.mountNFS(ctx, target, volume.Path, options); err != nil {
		return fmt.Errorf("failed to mount NFS at %q: %w", volume.Path, err)
	}

	a.storeMount(volume.Path, mountRecord{LifecycleID: lifecycleID, SourceType: Nfs})

	return nil
}

// setupBlock mounts one block volume: the guest device is found by filesystem
// UUID (the host attaches the drive, so the device name is not known in
// advance), its size is checked against the declared capacity, and it is mounted
// as ext4 at the path. The device is remembered so the mount-set freeze and
// /volumes/resize can act on it without a caller naming a device.
func (a *API) setupBlock(ctx context.Context, logger zerolog.Logger, lifecycleID string, volume VolumeMount) error {
	uuid := strings.ToLower(derefString(volume.FsUuid))

	device, err := resolveBlockDevice(ctx, uuid)
	if err != nil {
		return fmt.Errorf("failed to resolve block volume %q for %q: %w", uuid, volume.Path, err)
	}

	if volume.CapacityBytes != nil && *volume.CapacityBytes > 0 {
		size, err := blockDeviceSize(device)
		if err != nil {
			return fmt.Errorf("failed to read the size of %s for %q: %w", device, volume.Path, err)
		}
		if size < *volume.CapacityBytes {
			return fmt.Errorf("%w: %s is %d bytes, %q declares %d", ErrBlockDeviceTooSmall, device, size, volume.Path, *volume.CapacityBytes)
		}
		if size > *volume.CapacityBytes {
			// Larger is what a pause-then-resize leaves behind until /volumes/resize
			// runs; not an error, but worth a line.
			logger.Debug().Msgf("Block device %s is %d bytes, larger than the declared %d for %q", device, size, *volume.CapacityBytes, volume.Path)
		}
	}

	logger.Debug().
		Str("device", device).
		Str("device_id", derefString(volume.DeviceId)).
		Msgf("Setting up block volume %s at %q", uuid, volume.Path)

	commands := [][]string{
		{"mkdir", "-p", volume.Path},
		{"mount", "-t", "ext4", device, volume.Path},
	}

	for _, command := range commands {
		data, err := exec.CommandContext(ctx, command[0], command[1:]...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("`%s` failed: %w\n%s", strings.Join(command, " "), err, string(data))
		}
	}

	a.storeMount(volume.Path, mountRecord{LifecycleID: lifecycleID, SourceType: Block, Device: device})

	return nil
}

// resolveBlockDevice maps a filesystem UUID to the guest device node. The udev
// symlink is tried first because it needs no process; a guest without udev falls
// back to blkid -U, which scans the devices directly.
func resolveBlockDevice(ctx context.Context, uuid string) (string, error) {
	link := filepath.Join("/dev/disk/by-uuid", uuid)
	if target, err := filepath.EvalSymlinks(link); err == nil {
		return target, nil
	}

	data, err := exec.CommandContext(ctx, "blkid", "-U", uuid).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 2 {
			// blkid exits 2 when nothing matches.
			return "", fmt.Errorf("%w: no device carries UUID %s", ErrBlockDeviceNotFound, uuid)
		}

		return "", fmt.Errorf("blkid -U %s: %w", uuid, err)
	}

	device := strings.TrimSpace(string(data))
	if device == "" {
		return "", fmt.Errorf("%w: no device carries UUID %s", ErrBlockDeviceNotFound, uuid)
	}

	return device, nil
}

// blockDeviceSize reads the device size from sysfs (sectors of 512 bytes), which
// needs no helper binary in the guest.
func blockDeviceSize(device string) (int64, error) {
	resolved, err := filepath.EvalSymlinks(device)
	if err != nil {
		return 0, err
	}

	data, err := os.ReadFile(filepath.Join("/sys/class/block", filepath.Base(resolved), "size"))
	if err != nil {
		return 0, err
	}

	sectors, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse sector count: %w", err)
	}

	return sectors * 512, nil
}

func (a *API) unmountStale(ctx context.Context, logger zerolog.Logger, path string) error {
	// Check if actually mounted before trying to unmount.
	// findmnt returns exit code 1 when path is not a mount point - that's not an error.
	data, err := exec.CommandContext(ctx, "findmnt", "--noheadings", "--output", "SOURCE", path).CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			// Not mounted - nothing to unmount
			return nil
		}

		return fmt.Errorf("failed to check if %q is mounted: %w", path, err)
	}

	source := strings.TrimSpace(string(data))
	if source == "" {
		return nil // already unmounted
	}

	logger.Debug().Msgf("Unmounting stale mount at %q (was: %s)", path, source)

	if data, err = exec.CommandContext(ctx, "umount", "--force", path).CombinedOutput(); err != nil {
		logger.Warn().Err(err).Str("path", path).Str("output", string(data)).Msg("Forced umount failed, falling back to lazy")
		// Fresh ctx so the forced umount running out of budget doesn't kill the fallback.
		lazyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		if data, err = exec.CommandContext(lazyCtx, "umount", "--lazy", path).CombinedOutput(); err != nil {
			return fmt.Errorf("failed to unmount stale mount at %q: %w\n%s", path, err, string(data))
		}
	}

	a.mountedPaths.Delete(path)

	return nil
}

func (a *API) mountNFS(ctx context.Context, nfsTarget, path, options string) error {
	commands := [][]string{
		{"mkdir", "-p", path},
		{"mount", "-v", "-t", "nfs", "-o", "fg,hard," + options, nfsTarget, path},
	}

	for _, command := range commands {
		data, err := exec.CommandContext(ctx, command[0], command[1:]...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("`%s` failed: %w\n%s", strings.Join(command, " "), err, string(data))
		}
	}

	return nil
}

// shouldRemountNFS determines if a volume should be remounted based on lifecycle IDs.
// Returns true if remount is needed, false if we should skip (already mounted for this lifecycle).
//
// Truth table (treating nil/empty as equivalent):
//   - mounted="" + request="" → false (no remount - would cause infinite loop)
//   - mounted="abc" + request="" → true (remount - lifecycle cleared)
//   - mounted="" + request="abc" → true (remount - new lifecycle)
//   - mounted="abc" + request="abc" → false (no remount - same lifecycle)
//   - mounted="abc" + request="xyz" → true (remount - different lifecycle)
func shouldRemountNFS(isMounted bool, mountedLifecycleID, requestLifecycleID string) bool {
	if !isMounted {
		return true // not mounted yet, need to mount
	}

	return mountedLifecycleID != requestLifecycleID
}

// loadMount reads one ledger entry. A legacy string value (a lifecycle id stored
// by code predating mountRecord, e.g. a handover from an older envd) is read as
// an NFS mount.
func (a *API) loadMount(path string) (mountRecord, bool) {
	v, ok := a.mountedPaths.Load(path)
	if !ok {
		return mountRecord{}, false
	}

	switch r := v.(type) {
	case mountRecord:
		return r, true
	case string:
		return mountRecord{LifecycleID: r, SourceType: Nfs}, true
	default:
		return mountRecord{}, true
	}
}

func (a *API) storeMount(path string, record mountRecord) {
	if record.SourceType == "" {
		record.SourceType = Nfs
	}
	a.mountedPaths.Store(path, record)
}

// blockMountpoints returns the block-volume mountpoints from the ledger, deepest
// path first (a child mount freezes before its parent), ties broken lexically
// so the order is stable.
func (a *API) blockMountpoints() []string {
	var paths []string
	a.mountedPaths.Range(func(k, v any) bool {
		path, _ := k.(string)
		if r, ok := v.(mountRecord); ok && r.SourceType == Block && path != "" {
			paths = append(paths, filepath.Clean(path))
		}

		return true
	})

	slices.SortFunc(paths, func(x, y string) int {
		dx, dy := strings.Count(x, "/"), strings.Count(y, "/")
		if dx != dy {
			return dy - dx // deeper first
		}

		return strings.Compare(y, x)
	})

	return slices.Compact(paths)
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}

	return *p
}

// PostVolumesResize grows the ext4 filesystem of a mounted block volume to the
// size of its device (online resize2fs). It is the guest half of the
// pause-then-resize protocol: the host enlarged the backing image while the VM was
// paused and re-attached it; the kernel sees the larger device, and only
// resize2fs inside the guest can grow a mounted filesystem safely. The device
// comes from envd's own ledger, never from the request.
func (a *API) PostVolumesResize(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	logger := a.logger.With().Str(string(logs.OperationIDKey), logs.AssignOperationID()).Logger()

	var req VolumeResizeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, fmt.Errorf("decode request: %w", err))

		return
	}

	path := filepath.Clean(req.Path)
	record, ok := a.loadMount(path)
	if !ok || record.SourceType != Block || record.Device == "" {
		jsonError(w, http.StatusBadRequest, fmt.Errorf("%w: %q", ErrNotABlockVolume, req.Path))

		return
	}

	// resize2fs on a frozen filesystem blocks until the thaw, and a freeze that
	// lands mid-resize would snapshot a half-grown superblock; sharing the freeze
	// lock serializes the two.
	if err := a.fsFreezeLock.Acquire(r.Context(), 1); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)

		return
	}
	defer a.fsFreezeLock.Release(1)

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), resizeTimeout)
	defer cancel()

	logger.Info().Str("device", record.Device).Str("path", path).Msg("resizing block volume")

	data, err := exec.CommandContext(ctx, "resize2fs", record.Device).CombinedOutput()
	if err != nil {
		logger.Error().Err(err).Str("output", string(data)).Msg("resize2fs failed")
		jsonError(w, http.StatusInternalServerError, fmt.Errorf("resize2fs %s: %w\n%s", record.Device, err, string(data)))

		return
	}

	output := string(data)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(VolumeResizeResult{
		Path:   path,
		Device: record.Device,
		Output: &output,
	}); err != nil {
		logger.Error().Err(err).Msg("encode resize result")
	}
}
