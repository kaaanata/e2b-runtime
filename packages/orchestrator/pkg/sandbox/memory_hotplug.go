//go:build linux

package sandbox

import (
	"context"
	"errors"
	"fmt"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/metadata"
	"github.com/e2b-dev/infra/packages/shared/pkg/units"
)

// ErrMemoryNotHotResizable reports a memory size request for a sandbox whose
// template has no virtio-mem device: its memory is fixed at the boot size.
var ErrMemoryNotHotResizable = errors.New("sandbox memory is not hot-resizable: its template was built without maxMemoryMB")

// memfileSizeBytes is the size of the memory file a sandbox of this config
// boots with: boot memory, plus the whole hotplug region when the template
// has a virtio-mem device (its memory file covers the region, holes read as
// zeros).
func (c *Config) memfileSizeBytes() int64 {
	if c.MemoryHotplug != nil {
		return c.MemoryHotplug.MemfileSizeBytes()
	}

	return units.MBToBytes(c.RamMB)
}

// MemfileSizeBytes is memfileSizeBytes for callers that create the empty
// memory file of a cold boot (template builds).
func (c *Config) MemfileSizeBytes() int64 {
	return c.memfileSizeBytes()
}

// VerifyMemoryHotplug checks that the guest activated the sandbox's virtio-mem
// device, by requesting the size the template build runs at (0 plugged).
// Firecracker refuses the request when the guest driver never acknowledged
// the device, which is what a guest kernel without CONFIG_VIRTIO_MEM does; a
// template build must fail then instead of producing a template whose
// sandboxes cannot be resized. A no-op for a sandbox without a device.
func (s *Sandbox) VerifyMemoryHotplug(ctx context.Context) error {
	if s.Config.MemoryHotplug == nil {
		return nil
	}

	if err := s.process.PatchMemoryHotplug(ctx, 0); err != nil {
		return fmt.Errorf("memory hotplug: the guest kernel must have CONFIG_VIRTIO_MEM to build a template with maxMemoryMB: %w", err)
	}

	return nil
}

// adoptMemoryHotplug reconciles the config with the snapshot it starts from.
// The snapshot is authoritative for the device (it carries it in its device
// table and memory file); a config that names one must name the same one,
// and the boot memory must be the device's base.
func (c *Config) adoptMemoryHotplug(meta metadata.Template) error {
	if meta.MemoryHotplug == nil {
		if c.MemoryHotplug != nil {
			return fmt.Errorf("%w: the snapshot has no memory hotplug device", ErrMemoryNotHotResizable)
		}

		return nil
	}

	if c.MemoryHotplug != nil && !c.MemoryHotplug.Equal(meta.MemoryHotplug) {
		return fmt.Errorf("memory hotplug layout %+v does not match the snapshot's %+v", *c.MemoryHotplug, *meta.MemoryHotplug)
	}

	if c.RamMB != meta.MemoryHotplug.BaseMiB {
		return fmt.Errorf("boot memory %d MiB does not match the snapshot's %d MiB", c.RamMB, meta.MemoryHotplug.BaseMiB)
	}

	c.MemoryHotplug = meta.MemoryHotplug.Clone()

	return nil
}
