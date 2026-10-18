//go:build linux

package server

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/metadata"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
)

// cpuRefusalStatus maps a vCPU count refusal (the template has no CPU
// hotplug, or the count is outside its range) to FailedPrecondition, which the
// API answers with 409; nil when err is not one.
func cpuRefusalStatus(err error) error {
	if errors.Is(err, sandbox.ErrCPUNotHotResizable) || errors.Is(err, metadata.ErrCPUCountOutOfRange) {
		return status.Error(codes.FailedPrecondition, err.Error())
	}

	return nil
}

// ResizeCpu changes the vCPU count of a running sandbox of a CPU-hotplug
// template: the cgroup quota moves first, then the guest brings CPUs online
// or offline. The response reports the granted and the online count.
func (s *Server) ResizeCpu(ctx context.Context, req *orchestrator.SandboxResizeCpuRequest) (*orchestrator.SandboxResizeCpuResponse, error) {
	releaseWork := s.info.TrackWork()
	defer releaseWork()

	ctx, childSpan := tracer.Start(ctx, "sandbox-resize-cpu")
	defer childSpan.End()

	childSpan.SetAttributes(telemetry.WithSandboxID(req.GetSandboxId()))

	sbx, ok := s.sandboxFactory.Sandboxes.Get(req.GetSandboxId())
	if !ok {
		return nil, status.Error(codes.NotFound, "sandbox not found")
	}

	childSpan.SetAttributes(
		telemetry.WithTeamID(sbx.Runtime.TeamID),
		telemetry.WithTemplateID(sbx.Runtime.TemplateID),
		telemetry.WithBuildID(sbx.Runtime.BuildID),
	)

	if req.GetRequestedVcpu() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "requested_vcpu must be positive")
	}

	res, err := sbx.ResizeCPU(ctx, req.GetRequestedVcpu())
	if err != nil {
		if st := cpuRefusalStatus(err); st != nil {
			telemetry.ReportError(ctx, "vCPU resize refused", err, telemetry.WithSandboxID(req.GetSandboxId()))

			return nil, st
		}

		telemetry.ReportCriticalError(ctx, "failed to resize sandbox vCPUs", err, telemetry.WithSandboxID(req.GetSandboxId()))

		return nil, status.Errorf(codes.Internal, "failed to resize sandbox vCPUs: %s", err)
	}

	return &orchestrator.SandboxResizeCpuResponse{
		VcpuCount:  res.VCPUs,
		OnlineVcpu: res.OnlineVCPUs,
		MaxVcpu:    res.MaxVCPUs,
		Complete:   res.Complete,
	}, nil
}
