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

// ErrCodeCPUNotHotResizable (409): the sandbox's template has no CPU hot
// resize (built without maxCpuCount), or the node refused the count for it.
const ErrCodeCPUNotHotResizable = "cpu_not_hot_resizable"

// ResizeSandboxCPU changes the vCPU count of a running sandbox on its node and
// records the outcome in the store. The node is asked first; it moves the
// cgroup quota (the enforcement) before it returns, and reports how many CPUs
// the guest brought online.
func (o *Orchestrator) ResizeSandboxCPU(ctx context.Context, teamID uuid.UUID, sandboxID string, vcpus int64) (sandbox.Sandbox, *api.APIError) {
	ctx, span := tracer.Start(ctx, "resize-sandbox-cpu",
		trace.WithAttributes(
			attribute.String("instance.id", sandboxID),
			attribute.Int64("vcpu", vcpus),
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
	res, err := client.Sandbox.ResizeCpu(ctx, &orchestratorgrpc.SandboxResizeCpuRequest{
		SandboxId:     sandboxID,
		RequestedVcpu: vcpus,
	})
	if err != nil {
		return sandbox.Sandbox{}, resizeCPUError(sandboxID, err)
	}

	telemetry.ReportEvent(ctx, "Resized sandbox vCPUs on node",
		attribute.Int64("cpu_hotplug.vcpu", res.GetVcpuCount()),
		attribute.Int64("cpu_hotplug.online", res.GetOnlineVcpu()),
		attribute.Bool("cpu_hotplug.complete", res.GetComplete()),
	)

	// The node's allocation (best-of-k's input) picks the new count up with
	// its next metrics report, as for a memory resize.

	updated, err := o.sandboxStore.Update(ctx, teamID, sandboxID, func(s sandbox.Sandbox) (sandbox.Sandbox, error) {
		s.CPUCount = res.GetVcpuCount()
		s.CurrentCPUCount = res.GetOnlineVcpu()
		if s.MaxCPUCount == nil && res.GetMaxVcpu() > 0 {
			s.MaxCPUCount = new(res.GetMaxVcpu())
		}

		return s, nil
	})
	if err != nil {
		telemetry.ReportCriticalError(ctx, "failed to record sandbox vCPU resize", err)

		return sandbox.Sandbox{}, &api.APIError{Code: http.StatusInternalServerError, ClientMsg: "Error recording sandbox vCPUs", Err: err}
	}

	return updated, nil
}

// resizeCPUError maps the node's answer like resizeMemoryError does.
func resizeCPUError(sandboxID string, err error) *api.APIError {
	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.NotFound:
			return &api.APIError{Code: http.StatusNotFound, ClientMsg: utils.SandboxNotFoundMsg(sandboxID), Err: err}
		case codes.FailedPrecondition:
			return &api.APIError{Code: http.StatusConflict, ErrorCode: ErrCodeCPUNotHotResizable, ClientMsg: st.Message(), Err: err}
		case codes.InvalidArgument:
			return &api.APIError{Code: http.StatusBadRequest, ClientMsg: st.Message(), Err: err}
		}
	}

	err = utils.UnwrapGRPCError(err)

	return &api.APIError{
		Code:      http.StatusInternalServerError,
		ClientMsg: "Error resizing sandbox vCPUs",
		Err:       fmt.Errorf("failed to resize vCPUs of sandbox '%s': %w", sandboxID, err),
	}
}
