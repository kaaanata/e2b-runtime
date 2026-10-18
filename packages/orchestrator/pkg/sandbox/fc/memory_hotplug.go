//go:build linux

package fc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/metadata"
	"github.com/e2b-dev/infra/packages/shared/pkg/fc/client/operations"
	"github.com/e2b-dev/infra/packages/shared/pkg/fc/models"
)

// memhpOnlineMovableArg makes the guest online hotplugged memory into
// ZONE_MOVABLE. With the default onlining it lands in ZONE_NORMAL, where
// unmovable kernel allocations end up inside hotplugged blocks and unplug to 0
// stalls (measured on the fork FC with the vmem kernel: stuck at 4-8 MiB).
const (
	memhpDefaultStateParam = "memhp_default_state"
	memhpOnlineMovable     = "online_movable"
)

// MemoryHotplugStatus is the device state Firecracker reports, in MiB.
type MemoryHotplugStatus struct {
	TotalMiB     int64
	PluggedMiB   int64
	RequestedMiB int64
}

// ErrMemoryHotplugInactive reports a PATCH Firecracker refused because the
// guest never activated the virtio-mem device (a guest kernel without
// CONFIG_VIRTIO_MEM): the template's kernel cannot hot-resize.
var ErrMemoryHotplugInactive = errors.New("memory hotplug device is not active in the guest")

func (c *apiClient) setMemoryHotplug(ctx context.Context, l metadata.MemoryHotplugLayout) error {
	params := operations.PutMemoryHotplugParams{
		Context: ctx,
		Body: &models.MemoryHotplugConfig{
			TotalSizeMib: l.TotalMiB,
			SlotSizeMib:  l.SlotMiB,
			BlockSizeMib: l.BlockMiB,
		},
	}

	if _, err := c.client.Operations.PutMemoryHotplug(&params); err != nil {
		return fmt.Errorf("error configuring memory hotplug device: %w", err)
	}

	return nil
}

func (c *apiClient) patchMemoryHotplug(ctx context.Context, requestedMiB int64) error {
	params := operations.PatchMemoryHotplugParams{
		Context: ctx,
		Body:    &models.MemoryHotplugSizeUpdate{RequestedSizeMib: &requestedMiB},
	}

	if _, err := c.client.Operations.PatchMemoryHotplug(&params); err != nil {
		var defErr *operations.PatchMemoryHotplugDefault
		if errors.As(err, &defErr) {
			msg := fcErrorMessage(defErr.Payload)
			// Firecracker answers 400 "Device is not active" when the guest
			// driver never acknowledged the device.
			if defErr.Code() == http.StatusBadRequest && strings.Contains(strings.ToLower(msg), "not active") {
				return fmt.Errorf("%w: %s", ErrMemoryHotplugInactive, msg)
			}

			return fmt.Errorf("error requesting %d MiB of hotplug memory: %d %s", requestedMiB, defErr.Code(), msg)
		}

		return fmt.Errorf("error requesting %d MiB of hotplug memory: %w", requestedMiB, err)
	}

	return nil
}

func (c *apiClient) memoryHotplugStatus(ctx context.Context) (MemoryHotplugStatus, error) {
	params := operations.GetMemoryHotplugParams{Context: ctx}

	res, err := c.client.Operations.GetMemoryHotplug(&params)
	if err != nil {
		return MemoryHotplugStatus{}, fmt.Errorf("error getting memory hotplug status: %w", err)
	}

	if res.Payload == nil {
		return MemoryHotplugStatus{}, errors.New("memory hotplug status carried no payload")
	}

	return MemoryHotplugStatus{
		TotalMiB:     res.Payload.TotalSizeMib,
		PluggedMiB:   res.Payload.PluggedSizeMib,
		RequestedMiB: res.Payload.RequestedSizeMib,
	}, nil
}

func fcErrorMessage(e *models.Error) string {
	if e == nil {
		return "no error message"
	}

	return e.FaultMessage
}

// PatchMemoryHotplug asks the guest to plug or unplug hotplug memory until
// requestedMiB is plugged. It returns once Firecracker accepted the request;
// the guest driver does the plugging, so poll MemoryHotplugStatus for
// completion. Accepted on a paused VM too, and applied when it resumes.
func (p *Process) PatchMemoryHotplug(ctx context.Context, requestedMiB int64) error {
	return p.client.patchMemoryHotplug(ctx, requestedMiB)
}

// MemoryHotplugStatus reports the device's total, plugged and requested size.
// Works while the VM is paused.
func (p *Process) MemoryHotplugStatus(ctx context.Context) (MemoryHotplugStatus, error) {
	return p.client.memoryHotplugStatus(ctx)
}

// SetResumeMemoryHotplug makes Resume request requestedMiB of hotplug memory
// after the snapshot is loaded and before the vCPUs run, so the guest starts
// plugging (or unplugging) as soon as it resumes. Must be set before Resume.
func (p *Process) SetResumeMemoryHotplug(requestedMiB int64) {
	p.resumeHotplugMiB = &requestedMiB
}
