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

// memoryRefusalStatus maps a memory size refusal (the template has no hotplug
// device, or the size is outside its range) to FailedPrecondition, which the
// API answers with 409; nil when err is not one.
func memoryRefusalStatus(err error) error {
	switch {
	case errors.Is(err, sandbox.ErrMemoryNotHotResizable),
		errors.Is(err, metadata.ErrMemorySizeOutOfRange):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, sandbox.ErrMemoryResizeBusy):
		return status.Error(codes.Unavailable, err.Error())
	default:
		return nil
	}
}

// ResizeMemory changes the guest memory of a running sandbox (virtio-mem). The
// response reports the achieved size; an incomplete shrink is not an error
// (complete=false), the guest finishes on its own once it frees the memory.
func (s *Server) ResizeMemory(ctx context.Context, req *orchestrator.SandboxResizeMemoryRequest) (*orchestrator.SandboxResizeMemoryResponse, error) {
	releaseWork := s.info.TrackWork()
	defer releaseWork()

	ctx, childSpan := tracer.Start(ctx, "sandbox-resize-memory")
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

	if req.GetRequestedMemoryMb() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "requested_memory_mb must be positive")
	}

	res, err := sbx.ResizeMemory(ctx, req.GetRequestedMemoryMb(), sandbox.DefaultMemoryHotplugTimeout)
	if err != nil {
		if st := memoryRefusalStatus(err); st != nil {
			telemetry.ReportError(ctx, "memory resize refused", err, telemetry.WithSandboxID(req.GetSandboxId()))

			return nil, st
		}

		telemetry.ReportCriticalError(ctx, "failed to resize sandbox memory", err, telemetry.WithSandboxID(req.GetSandboxId()))

		return nil, status.Errorf(codes.Internal, "failed to resize sandbox memory: %s", err)
	}

	return &orchestrator.SandboxResizeMemoryResponse{
		RequestedMemoryMb: res.RequestedMB,
		MemoryMb:          res.MemoryMB,
		MaxMemoryMb:       res.MaxMB,
		Complete:          res.Complete,
	}, nil
}
