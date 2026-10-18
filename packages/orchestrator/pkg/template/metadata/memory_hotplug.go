package metadata

import (
	"errors"
	"fmt"
	"slices"
)

// CapabilityMemoryHotplugV1 is the required capability of a template whose
// snapshots carry a virtio-mem hotplug device (see Template.MemoryHotplug). A
// reader without this code refuses such a template at decode instead of
// restoring a snapshot whose memory file and UFFD handshake it would
// mis-handle (it sizes and translates guest memory as boot RAM only).
const CapabilityMemoryHotplugV1 = "memory-hotplug-v1"

const (
	// DefaultMemoryHotplugSlotMiB is the slot size of the hotplug region: the
	// unit Firecracker maps into KVM (one memslot per slot), and the unit the
	// guest unplugs before host memory can be returned. 128 MiB is
	// Firecracker's minimum and the guest's memory block size.
	DefaultMemoryHotplugSlotMiB int64 = 128

	// DefaultMemoryHotplugBlockMiB is the plug/unplug granularity inside a slot.
	// 2 MiB keeps every block hugepage-aligned, so the hotplug region can be
	// backed by the same 2 MiB pages as boot memory.
	DefaultMemoryHotplugBlockMiB int64 = 2

	// minMemoryHotplugSlotMiB and minMemoryHotplugBlockMiB are Firecracker's
	// lower bounds (PUT /hotplug/memory).
	minMemoryHotplugSlotMiB  int64 = 128
	minMemoryHotplugBlockMiB int64 = 2
)

// MemoryHotplugLayout describes the virtio-mem device a template's base
// snapshot was built with. It is lineage state: the device and the guest
// physical/memfile layout are fixed when the base snapshot is taken, every
// later snapshot of the lineage restores them.
//
// The memory file of such a template is laid out as
//
//	[0, BaseMiB)                   boot memory (machine-config mem_size_mib)
//	[BaseMiB, BaseMiB + TotalMiB)  hotplug region, plugged in BlockMiB blocks
//
// and is BaseMiB + TotalMiB long; never-plugged or unplugged ranges are holes
// (zero on read).
type MemoryHotplugLayout struct {
	// BaseMiB is the boot memory, the always-present minimum. It equals the
	// template's memoryMB; recorded so a reader can verify a start request
	// against the snapshot instead of trusting the caller.
	BaseMiB int64 `json:"base_mib"`
	// TotalMiB is the size of the hotplug region (maximum minus base).
	TotalMiB int64 `json:"total_mib"`
	// SlotMiB is the Firecracker slot size.
	SlotMiB int64 `json:"slot_mib"`
	// BlockMiB is the virtio-mem block size.
	BlockMiB int64 `json:"block_mib"`
}

// ErrMemoryHotplugLayout is matched by every layout validation error.
var ErrMemoryHotplugLayout = errors.New("invalid memory hotplug layout")

// NewMemoryHotplugLayout returns the layout of a template with boot memory
// baseMiB that may grow to maxMiB, with the default slot and block sizes. It
// returns nil (no device) when maxMiB is not above baseMiB: a template whose
// maximum equals its base has nothing to plug.
func NewMemoryHotplugLayout(baseMiB, maxMiB int64) (*MemoryHotplugLayout, error) {
	if maxMiB < baseMiB {
		return nil, fmt.Errorf("%w: maximum memory %d MiB is below the boot memory %d MiB", ErrMemoryHotplugLayout, maxMiB, baseMiB)
	}

	if maxMiB == baseMiB {
		return nil, nil
	}

	l := &MemoryHotplugLayout{
		BaseMiB:  baseMiB,
		TotalMiB: maxMiB - baseMiB,
		SlotMiB:  DefaultMemoryHotplugSlotMiB,
		BlockMiB: DefaultMemoryHotplugBlockMiB,
	}
	if err := l.Validate(); err != nil {
		return nil, err
	}

	return l, nil
}

// Validate checks the layout against Firecracker's device rules.
func (l *MemoryHotplugLayout) Validate() error {
	switch {
	case l.BaseMiB <= 0:
		return fmt.Errorf("%w: boot memory %d MiB", ErrMemoryHotplugLayout, l.BaseMiB)
	case l.BlockMiB < minMemoryHotplugBlockMiB || l.BlockMiB&(l.BlockMiB-1) != 0:
		return fmt.Errorf("%w: block size %d MiB must be a power of two of at least %d MiB", ErrMemoryHotplugLayout, l.BlockMiB, minMemoryHotplugBlockMiB)
	case l.SlotMiB < minMemoryHotplugSlotMiB || l.SlotMiB%l.BlockMiB != 0:
		return fmt.Errorf("%w: slot size %d MiB must be at least %d MiB and a multiple of the block size", ErrMemoryHotplugLayout, l.SlotMiB, minMemoryHotplugSlotMiB)
	case l.TotalMiB <= 0 || l.TotalMiB%l.SlotMiB != 0:
		return fmt.Errorf("%w: hotplug region %d MiB (maximum minus boot memory) must be a positive multiple of the %d MiB slot size",
			ErrMemoryHotplugLayout, l.TotalMiB, l.SlotMiB)
	case l.BaseMiB%l.BlockMiB != 0:
		// The region starts right after boot memory in the memory file; keep it
		// block (and so hugepage) aligned.
		return fmt.Errorf("%w: boot memory %d MiB must be a multiple of the %d MiB block size", ErrMemoryHotplugLayout, l.BaseMiB, l.BlockMiB)
	}

	return nil
}

// MaxMiB is the largest guest memory the template can be resized to.
func (l *MemoryHotplugLayout) MaxMiB() int64 {
	return l.BaseMiB + l.TotalMiB
}

// MemfileSizeBytes is the size of the template's memory file: boot memory
// plus the whole hotplug region.
func (l *MemoryHotplugLayout) MemfileSizeBytes() int64 {
	return l.MaxMiB() << 20
}

// HotplugOffsetBytes is the memory-file offset where the hotplug region starts.
func (l *MemoryHotplugLayout) HotplugOffsetBytes() int64 {
	return l.BaseMiB << 20
}

// HotplugSizeBytes is the size of the hotplug region in bytes.
func (l *MemoryHotplugLayout) HotplugSizeBytes() int64 {
	return l.TotalMiB << 20
}

// ErrMemorySizeOutOfRange is matched by RequestedHotplugMiB's errors.
var ErrMemorySizeOutOfRange = errors.New("memory size out of range")

// RequestedHotplugMiB converts a requested guest memory size into the hotplug
// size to request from the device: memoryMB minus the boot memory, rounded UP
// to the block size (the device only accepts whole blocks, and rounding down
// would give the guest less than asked for). memoryMB must lie within
// [BaseMiB, MaxMiB]; the maximum is slot- and therefore block-aligned, so the
// rounded value never exceeds the region.
func (l *MemoryHotplugLayout) RequestedHotplugMiB(memoryMB int64) (int64, error) {
	if memoryMB < l.BaseMiB || memoryMB > l.MaxMiB() {
		return 0, fmt.Errorf("%w: %d MiB is outside [%d, %d] MiB", ErrMemorySizeOutOfRange, memoryMB, l.BaseMiB, l.MaxMiB())
	}

	hotplug := memoryMB - l.BaseMiB
	if rem := hotplug % l.BlockMiB; rem != 0 {
		hotplug += l.BlockMiB - rem
	}

	return hotplug, nil
}

// AccountedMiB is the guest memory a node must account for a sandbox of this
// layout: boot memory plus the larger of the requested and the plugged
// hotplug size. While a shrink is pending (or stalled, the guest cannot
// offline the blocks) the memory is still plugged; while a grow is pending
// the request is already promised.
func (l *MemoryHotplugLayout) AccountedMiB(requestedHotplugMiB, pluggedHotplugMiB int64) int64 {
	return l.BaseMiB + max(requestedHotplugMiB, pluggedHotplugMiB)
}

// Equal reports whether two layouts (either may be nil) describe the same device.
func (l *MemoryHotplugLayout) Equal(o *MemoryHotplugLayout) bool {
	if l == nil || o == nil {
		return l == nil && o == nil
	}

	return *l == *o
}

// Clone returns a copy of the layout (nil for nil).
func (l *MemoryHotplugLayout) Clone() *MemoryHotplugLayout {
	if l == nil {
		return nil
	}

	c := *l

	return &c
}

// HasMemoryHotplug reports whether this template's snapshots carry a virtio-mem
// device, i.e. its sandboxes' memory can be resized while they run.
func (t Template) HasMemoryHotplug() bool {
	return t.MemoryHotplug != nil
}

// WithMemoryHotplug stamps the hotplug layout of the build that produced this
// metadata, together with the capability a reader must have to handle it.
// A nil layout clears both (the build has no device). Other required
// capabilities are kept.
func (t Template) WithMemoryHotplug(l *MemoryHotplugLayout) Template {
	t.MemoryHotplug = l.Clone()
	t.RequiredCapabilities = slices.DeleteFunc(slices.Clone(t.RequiredCapabilities), func(c string) bool {
		return c == CapabilityMemoryHotplugV1
	})

	if l != nil {
		t.RequiredCapabilities = append(t.RequiredCapabilities, CapabilityMemoryHotplugV1)
	}

	if len(t.RequiredCapabilities) == 0 {
		t.RequiredCapabilities = nil
	}

	return t
}

// MarkMemoryHotplugPlugged records the hotplug size that was plugged when
// this snapshot was taken (0 for a template without the device, or a
// filesystem-only snapshot). Per snapshot, like FilesystemOnly: the copy
// constructors other than WithPrefetch do not carry it.
func (t Template) MarkMemoryHotplugPlugged(pluggedMiB int64) Template {
	t.MemoryHotplugPluggedMiB = pluggedMiB

	return t
}
