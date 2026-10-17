package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/rs/zerolog"

	"github.com/e2b-dev/infra/packages/envd/internal/logs"
)

// Mount-set freeze: the checkpoint protocol for a sandbox with block volumes.
//
// A filesystem-only pause of the rootfs alone (PostFsfreeze) is not enough once
// the guest also writes block volumes: every ext4 the snapshot will carry has to
// be flushed and suspended before the host copies it. The set is derived by envd
// from its own ledger — the block volumes /init mounted, deepest path first so a
// child mount quiesces before its parent, then the rootfs — and never from the
// caller, so a compromised orchestrator credential cannot freeze an arbitrary
// path.
//
// The operation token exists because HTTP is lossy in the one direction that
// matters: a freeze that completed but whose 200 never reached the orchestrator
// has left the VM with its filesystems suspended. The orchestrator asks GET
// /fsfreeze-set what happened, and POST /fsthaw-set with the same token undoes
// exactly what froze. The registry is bounded: a token older than the last
// maxFreezeOps operations answers 404, and the thaw then falls back to the whole
// current set, which is safe because FITHAW on an unfrozen filesystem is a no-op.

const (
	maxFreezeOps        = 32
	maxFreezeTokenBytes = 128
)

var (
	ErrFreezeTokenInvalid = errors.New("operation_token must be 1 to 128 bytes")
	ErrFreezeOpUnknown    = errors.New("no mount-set freeze recorded under this token")
)

// freezeOps is the bounded registry of mount-set freeze results by token.
// Guarded by API.fsFreezeLock: every reader and writer holds it, which also
// serializes the freezes themselves.
type freezeOps struct {
	byToken map[string]FsFreezeSetResult
	order   []string
}

func (o *freezeOps) get(token string) (FsFreezeSetResult, bool) {
	if o.byToken == nil {
		return FsFreezeSetResult{}, false
	}
	r, ok := o.byToken[token]

	return r, ok
}

func (o *freezeOps) put(result FsFreezeSetResult) {
	if o.byToken == nil {
		o.byToken = make(map[string]FsFreezeSetResult)
	}
	if _, exists := o.byToken[result.OperationToken]; !exists {
		o.order = append(o.order, result.OperationToken)
		for len(o.order) > maxFreezeOps {
			delete(o.byToken, o.order[0])
			o.order = o.order[1:]
		}
	}
	o.byToken[result.OperationToken] = result
}

// freezeSet is the ordered mount set a checkpoint must quiesce: block volumes,
// deepest path first, then the rootfs last.
func (a *API) freezeSet() []string {
	return append(a.blockMountpoints(), rootfsMountpoint)
}

func decodeFreezeSetRequest(r *http.Request) (string, error) {
	var req FsFreezeSetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return "", fmt.Errorf("decode request: %w", err)
	}
	if req.OperationToken == "" || len(req.OperationToken) > maxFreezeTokenBytes {
		return "", ErrFreezeTokenInvalid
	}

	return req.OperationToken, nil
}

// PostFsfreezeSet freezes the mount set under an operation token. On a partial
// failure it thaws what had frozen (reverse order) and reports the whole picture
// as a 500 whose body is the same structured result, so the caller never has to
// infer which filesystems are suspended.
func (a *API) PostFsfreezeSet(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	logger := a.logger.With().Str(string(logs.OperationIDKey), logs.AssignOperationID()).Logger()

	token, err := decodeFreezeSetRequest(r)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err)

		return
	}

	if err := a.fsFreezeLock.Acquire(r.Context(), 1); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)

		return
	}
	defer a.fsFreezeLock.Release(1)

	// Idempotent per token: a retry of a POST whose response was lost gets the
	// recorded answer rather than a second freeze (which would be a no-op at the
	// kernel but would overwrite the record of what actually happened).
	if prev, ok := a.freezeOps.get(token); ok && prev.State == Frozen {
		writeFreezeSetResult(w, logger, http.StatusOK, prev)

		return
	}

	set := a.freezeSet()
	result := FsFreezeSetResult{
		OperationToken: token,
		State:          Frozen,
		Mountpoints:    &set,
		Frozen:         []string{},
	}

	for _, mp := range set {
		if err := a.fsFreezer.Freeze(mp); err != nil {
			logger.Error().Err(err).Str("mountpoint", mp).Msg("freeze mount set: mountpoint failed, rolling back")
			msg := err.Error()
			result.State = Failed
			result.FailedMountpoint = &mp
			result.Error = &msg
			a.thawInto(logger, &result, slices.Clone(result.Frozen))
			a.freezeOps.put(result)
			writeFreezeSetResult(w, logger, http.StatusInternalServerError, result)

			return
		}
		result.Frozen = append(result.Frozen, mp)
	}

	a.freezeOps.put(result)
	writeFreezeSetResult(w, logger, http.StatusOK, result)
}

// GetFsfreezeSet reports the recorded outcome for a token.
func (a *API) GetFsfreezeSet(w http.ResponseWriter, r *http.Request, params GetFsfreezeSetParams) {
	defer r.Body.Close()

	logger := a.logger.With().Str(string(logs.OperationIDKey), logs.AssignOperationID()).Logger()

	if params.OperationToken == "" || len(params.OperationToken) > maxFreezeTokenBytes {
		jsonError(w, http.StatusBadRequest, ErrFreezeTokenInvalid)

		return
	}

	// The lock is what makes the read consistent with an in-flight freeze: a
	// status asked mid-operation waits for the operation's record rather than
	// reading a half-written one.
	if err := a.fsFreezeLock.Acquire(r.Context(), 1); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)

		return
	}
	defer a.fsFreezeLock.Release(1)

	result, ok := a.freezeOps.get(params.OperationToken)
	if !ok {
		jsonError(w, http.StatusNotFound, fmt.Errorf("%w: %q", ErrFreezeOpUnknown, params.OperationToken))

		return
	}

	writeFreezeSetResult(w, logger, http.StatusOK, result)
}

// PostFsthawSet thaws the set a freeze under this token suspended, rootfs first
// (the reverse of the freeze order). An unknown token thaws the whole current
// set: the caller is in rollback and the one thing it must not do is leave a
// filesystem frozen because a record went missing.
func (a *API) PostFsthawSet(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	logger := a.logger.With().Str(string(logs.OperationIDKey), logs.AssignOperationID()).Logger()

	token, err := decodeFreezeSetRequest(r)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err)

		return
	}

	// WithoutCancel so a cancelled HTTP client can't abandon the thaw and leave
	// the live VM's filesystems frozen.
	if err := a.fsFreezeLock.Acquire(context.WithoutCancel(r.Context()), 1); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)

		return
	}
	defer a.fsFreezeLock.Release(1)

	result, known := a.freezeOps.get(token)
	var toThaw []string
	if known {
		toThaw = slices.Clone(result.Frozen)
	} else {
		logger.Warn().Str("token", token).Msg("thaw mount set: no freeze recorded under this token, thawing the whole current set")
		set := a.freezeSet()
		result = FsFreezeSetResult{
			OperationToken: token,
			Mountpoints:    &set,
			Frozen:         []string{},
		}
		toThaw = set
	}

	// A re-run of a thaw starts from scratch: the previous thaw_errors are what
	// this call retries, and a recorded success is simply re-confirmed.
	result.Thawed = nil
	result.ThawErrors = nil
	a.thawInto(logger, &result, toThaw)
	a.freezeOps.put(result)

	writeFreezeSetResult(w, logger, http.StatusOK, result)
}

// thawInto thaws frozen in reverse order (the last mountpoint frozen is the first
// thawed), appending each outcome to result. Every mountpoint is attempted even
// after a failure: a thaw that stops early leaves more frozen, never less. The
// state ends thawed only when nothing is left suspended; otherwise it keeps the
// state it had (frozen, or failed) and thaw_errors says what still is.
func (a *API) thawInto(logger zerolog.Logger, result *FsFreezeSetResult, frozen []string) {
	thawed := make([]string, 0, len(frozen))
	var thawErrors []string

	for _, mp := range slices.Backward(frozen) {
		if err := a.fsFreezer.Thaw(mp); err != nil {
			logger.Error().Err(err).Str("mountpoint", mp).Msg("thaw mount set: mountpoint failed")
			thawErrors = append(thawErrors, mp)

			continue
		}
		thawed = append(thawed, mp)
	}

	result.Thawed = &thawed
	if len(thawErrors) > 0 {
		result.ThawErrors = &thawErrors

		return
	}
	if result.State != Failed {
		result.State = Thawed
	}
}

func writeFreezeSetResult(w http.ResponseWriter, logger zerolog.Logger, status int, result FsFreezeSetResult) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(result); err != nil {
		logger.Error().Err(err).Msg("encode mount-set freeze result")
	}
}
