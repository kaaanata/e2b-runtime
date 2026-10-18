package constants

const (
	MinTemplateCPU        = int64(1)
	MaxTemplateCPU        = int64(32)
	MinTemplateMemory     = int64(128)
	DefaultTemplateCPU    = int64(2)
	DefaultTemplateMemory = int64(1024)

	// MemoryHotplugSlotMB is the granularity of a template's memory hotplug
	// region (maxMemoryMB - memoryMB). It mirrors the slot size the
	// template-manager configures, which re-validates the request.
	MemoryHotplugSlotMB = int64(128)
)
