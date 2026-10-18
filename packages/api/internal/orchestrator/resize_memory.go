package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/api/internal/sandbox"
	"github.com/e2b-dev/infra/packages/api/internal/utils"
	orchestratorgrpc "github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
)

// Machine-readable error codes of memory resize refusals (409).
const (
	// ErrCodeMemoryNotHotResizable: the sandbox's template has no memory
	// hotplug device (built without maxMemoryMB), or the node refused the
	// size for it.
	ErrCodeMemoryNotHotResizable = "memory_not_hot_resizable"
	// ErrCodeMemoryResizeBusy: a checkpoint of the sandbox is in progress;
	// retry after it.
	ErrCodeMemoryResizeBusy = "memory_resize_busy"
)

// ResizeSandboxMemory changes the guest memory of a running sandbox on its
// node and records the outcome in the store. The node is asked FIRST: a size
// the guest does not have must never be recorded, and the node reports what
// was achieved (a shrink can stall while the guest pins the memory; the
// request is still recorded, the guest finishes on its own).
func (o *Orchestrator) ResizeSandboxMemory(ctx context.Context, teamID uuid.UUID, sandboxID string, memoryMB int64) (sandbox.Sandbox, *api.APIError) {
	ctx, span := tracer.Start(ctx, "resize-sandbox-memory",
		trace.WithAttributes(
			attribute.String("instance.id", sandboxID),
			attribute.Int64("memory_mb", memoryMB),
		),
	)
	defer span.End()

	sbx, err := o.sandboxStore.Get(ctx, teamID, sandboxID)
	if err != nil {
		if errors.Is(err, sandbox.ErrNotFound) {
			return sandbox.Sandbox{}, &api.APIError{Code: http.StatusNotFound, ClientMsg: utils.SandboxNotFoundMsg(sandboxID), Err: err}
		}

		return sandbox.Sandbox{}, &api.APIError{Code: http.StatusInternalServerError, ClientMsg: "Error getting sandbox", Err: err}
	}

	if sbx.State != sandbox.StateRunning {
		notRunning := &sandbox.NotRunningError{SandboxID: sandboxID, State: sbx.State}

		return sandbox.Sandbox{}, &api.APIError{Code: http.StatusConflict, ClientMsg: utils.SandboxChangingStateMsg(sandboxID, sbx.State), Err: notRunning}
	}

	node := o.getOrConnectNode(ctx, sbx.ClusterID, sbx.NodeID)
	if node == nil {
		return sandbox.Sandbox{}, &api.APIError{
			Code:      http.StatusInternalServerError,
			ClientMsg: fmt.Sprintf("Node hosting sandbox '%s' not found", sandboxID),
			Err:       fmt.Errorf("node '%s' not found for cluster '%s'", sbx.NodeID, sbx.ClusterID),
		}
	}

	client, ctx := node.GetClient(ctx)
	res, err := client.Sandbox.ResizeMemory(ctx, &orchestratorgrpc.SandboxResizeMemoryRequest{
		SandboxId:         sandboxID,
		RequestedMemoryMb: memoryMB,
	})
	if err != nil {
		return sandbox.Sandbox{}, resizeMemoryError(sandboxID, err)
	}

	telemetry.ReportEvent(ctx, "Resized sandbox memory on node",
		attribute.Int64("memory_hotplug.memory_mb", res.GetMemoryMb()),
		attribute.Bool("memory_hotplug.complete", res.GetComplete()),
	)

	updated, err := o.sandboxStore.Update(ctx, teamID, sandboxID, func(s sandbox.Sandbox) (sandbox.Sandbox, error) {
		s.MemoryMB = res.GetRequestedMemoryMb()
		s.CurrentMemoryMB = res.GetMemoryMb()
		if s.MaxMemoryMB == nil && res.GetMaxMemoryMb() > 0 {
			s.MaxMemoryMB = new(res.GetMaxMemoryMb())
		}

		return s, nil
	})
	if err != nil {
		// The guest already has the new size; the record is behind until the
		// next node sync. Report it rather than pretend nothing happened.
		telemetry.ReportCriticalError(ctx, "failed to record sandbox memory resize", err)

		return sandbox.Sandbox{}, &api.APIError{Code: http.StatusInternalServerError, ClientMsg: "Error recording sandbox memory", Err: err}
	}

	return updated, nil
}

// resizeMemoryError maps the node's answer: FailedPrecondition is a refusal
// (no device / out of range), Unavailable a checkpoint in flight, NotFound a
// sandbox the node no longer runs.
func resizeMemoryError(sandboxID string, err error) *api.APIError {
	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.NotFound:
			return &api.APIError{Code: http.StatusNotFound, ClientMsg: utils.SandboxNotFoundMsg(sandboxID), Err: err}
		case codes.FailedPrecondition:
			return &api.APIError{Code: http.StatusConflict, ErrorCode: ErrCodeMemoryNotHotResizable, ClientMsg: st.Message(), Err: err}
		case codes.Unavailable:
			return &api.APIError{Code: http.StatusConflict, ErrorCode: ErrCodeMemoryResizeBusy, ClientMsg: st.Message(), Err: err}
		case codes.InvalidArgument:
			return &api.APIError{Code: http.StatusBadRequest, ClientMsg: st.Message(), Err: err}
		}
	}

	err = utils.UnwrapGRPCError(err)

	return &api.APIError{
		Code:      http.StatusInternalServerError,
		ClientMsg: "Error resizing sandbox memory",
		Err:       fmt.Errorf("failed to resize memory of sandbox '%s': %w", sandboxID, err),
	}
}
