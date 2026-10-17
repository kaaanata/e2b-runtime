//go:build linux

package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"

	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/envd"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

// errEnvdEndpointMissing marks a 404 from an envd endpoint that the running
// envd predates.
var errEnvdEndpointMissing = errors.New("envd endpoint not found")

// envdFreezeSetCall performs one mount-set freeze/thaw/status call and decodes
// the FsFreezeSetResult, which envd returns with 200 and (for a failed freeze)
// 500 alike.
func (s *Sandbox) envdFreezeSetCall(ctx context.Context, method, path, token string) (envd.FsFreezeSetResult, int, error) {
	ctx, cancel := context.WithTimeout(ctx, s.guestSyncTimeout(ctx))
	defer cancel()

	var (
		query string
		body  any
	)
	if method == http.MethodGet {
		query = url.Values{"operation_token": []string{token}}.Encode()
	} else {
		body = envd.FsFreezeSetRequest{OperationToken: token}
	}

	resp, err := s.doEnvdRequest(ctx, method, path, query, body)
	if err != nil {
		return envd.FsFreezeSetResult{}, 0, err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	if resp.StatusCode == http.StatusNotFound && method != http.MethodGet {
		return envd.FsFreezeSetResult{}, resp.StatusCode, fmt.Errorf("%w: %s", errEnvdEndpointMissing, path)
	}

	var result envd.FsFreezeSetResult
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusInternalServerError {
		if err := json.Unmarshal(raw, &result); err == nil && result.State != "" {
			return result, resp.StatusCode, nil
		}
	}

	return envd.FsFreezeSetResult{}, resp.StatusCode, fmt.Errorf("%s returned %d: %s", path, resp.StatusCode, utils.Truncate(string(raw), 200))
}

// freezeSetCovers reports whether a freeze result froze its whole set, and
// the set contains / and every bound workspace mount.
func freezeSetCovers(result envd.FsFreezeSetResult, mounts []string) error {
	if result.State != envd.Frozen {
		return fmt.Errorf("mount-set freeze is %q, not frozen (failed at %q: %s)", result.State, result.FailedMountpoint, result.Error)
	}

	want := append([]string{"/"}, mounts...)
	for _, m := range want {
		if !slices.Contains(result.Mountpoints, m) {
			return fmt.Errorf("mount-set freeze did not include %s (set %v)", m, result.Mountpoints)
		}
	}

	for _, m := range result.Mountpoints {
		if !slices.Contains(result.Frozen, m) {
			return fmt.Errorf("mount-set freeze reports %s unfrozen (frozen %v)", m, result.Frozen)
		}
	}

	return nil
}

// guestFreezeMountSetForPause is the filesystem-only pause quiesce of a
// sandbox with workspace volumes: every bound block mount and / are frozen
// through envd's /fsfreeze-set, so all of them are captured consistent. There
// is no sync fallback: a plain sync leaves the sync->pause race open, which a
// filesystem-quiesced revision must not have. A thaw under the same token is
// registered on cleanup BEFORE the freeze is attempted, so an aborted pause
// never leaves the live guest frozen (thawing an unfrozen filesystem is a
// no-op, and an unknown token thaws envd's whole current set).
//
// It returns nil only when envd reports every mountpoint of the set frozen.
func (s *Sandbox) guestFreezeMountSetForPause(ctx context.Context, cleanup *Cleanup, token string) error {
	ctx, span := tracer.Start(ctx, "envd-guest-mount-set-freeze")
	defer span.End()

	mounts := make([]string, 0, len(s.workspace.bound))
	for _, b := range s.workspace.bound {
		mounts = append(mounts, b.MountPath)
	}

	cleanup.Add(ctx, func(ctx context.Context) error {
		s.bestEffortThawMountSet(ctx, token)

		return nil
	})

	result, code, err := s.envdFreezeSetCall(ctx, http.MethodPost, "fsfreeze-set", token)
	switch {
	case errors.Is(err, errEnvdEndpointMissing):
		return fmt.Errorf("%w: filesystem-only pause needs envd's mount-set freeze (/fsfreeze-set), the running envd %s lacks it", ErrCapabilityUnsupported, s.Config.Envd.Version)
	case err != nil && code == 0:
		// The response was lost: the freeze may or may not have run. Ask envd
		// what the token recorded; anything short of a complete freeze aborts.
		status, _, getErr := s.envdFreezeSetCall(context.WithoutCancel(ctx), http.MethodGet, "fsfreeze-set", token)
		if getErr != nil {
			return fmt.Errorf("mount-set freeze before filesystem-only pause: %w (status unknown: %w)", err, getErr)
		}

		result = status
	case err != nil:
		return fmt.Errorf("mount-set freeze before filesystem-only pause: %w", err)
	}

	if err := freezeSetCovers(result, mounts); err != nil {
		return fmt.Errorf("mount-set freeze before filesystem-only pause: %w", err)
	}

	return nil
}

// thawMountSet thaws what a mount-set freeze froze and fails when any
// mountpoint stayed frozen.
func (s *Sandbox) thawMountSet(ctx context.Context, token string) error {
	result, _, err := s.envdFreezeSetCall(ctx, http.MethodPost, "fsthaw-set", token)
	if err != nil {
		return err
	}

	if len(result.ThawErrors) > 0 {
		return fmt.Errorf("mount-set thaw left %v frozen", result.ThawErrors)
	}

	return nil
}

// bestEffortThawMountSet is the rollback form of thawMountSet: it runs from
// cleanup paths whose context may be done, and logs instead of failing.
func (s *Sandbox) bestEffortThawMountSet(ctx context.Context, token string) {
	if err := s.thawMountSet(context.WithoutCancel(ctx), token); err != nil && !errors.Is(err, errEnvdEndpointMissing) {
		s.log().Warn(ctx, "envd mount-set thaw failed", zap.Error(err))
	}
}

// callEnvdVolumeResize asks envd to grow a mounted block volume's ext4 to its
// device size (online resize2fs).
func (s *Sandbox) callEnvdVolumeResize(ctx context.Context, mountPath string) error {
	ctx, cancel := context.WithTimeout(ctx, s.guestSyncTimeout(ctx))
	defer cancel()

	resp, err := s.doEnvdRequest(ctx, http.MethodPost, "volumes/resize", "", envd.VolumeResizeRequest{Path: mountPath})
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	switch resp.StatusCode {
	case http.StatusOK:
		var result envd.VolumeResizeResult
		if err := json.Unmarshal(raw, &result); err == nil {
			s.log().Info(ctx, "grew workspace volume online", zap.String("path", result.Path), zap.String("device", result.Device))
		}

		return nil
	case http.StatusNotFound:
		return fmt.Errorf("%w: envd %s has no /volumes/resize", ErrCapabilityUnsupported, s.Config.Envd.Version)
	default:
		return fmt.Errorf("volumes/resize returned %d: %s", resp.StatusCode, utils.Truncate(string(raw), 200))
	}
}
