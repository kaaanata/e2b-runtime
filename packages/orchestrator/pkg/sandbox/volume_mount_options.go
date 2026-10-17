package sandbox

import (
	"slices"
)

// Consistency profiles for shared (NFS) volumes. The profile is chosen per mount
// by the API and travels in SandboxVolumeMount.consistency_profile; the
// orchestrator turns it into the mount options envd passes to mount(8), and envd
// validates every option against its whitelist (see envd's VolumeMount schema).
//
// An empty profile sends no options at all, which makes envd use its built-in
// set: byte-for-byte what every sandbox created before profiles existed gets.
//
// The API's copy of the profile names is types.VolumeConsistencyStrict/Cto in
// packages/db/pkg/types/types.go; keep the two in sync.
const (
	// VolumeConsistencyStrict is envd's built-in option set plus nolock: every
	// operation goes to the server, no attribute or name caching. For small
	// amounts of metadata that must be visible across sandboxes at once.
	VolumeConsistencyStrict = "strict"
	// VolumeConsistencyCto is close-to-open: the built-in set without noac and
	// lookupcache=none, plus nolock. A writer's close makes its data visible to a
	// reader's next open. The default for repositories, dependency caches and
	// build outputs, where strict costs ~500x on metadata-heavy work.
	VolumeConsistencyCto = "cto"
)

// nfsBaseMountOptions mirrors envd's built-in option set (its nfsDefaultOptions).
// fg,hard are not listed because envd prepends them to every NFS mount. Kept here
// rather than imported because the two sides are released separately: this list
// is what this orchestrator asks for, envd's whitelist is what it will accept.
var nfsBaseMountOptions = []string{
	"sync",
	"rsize=1048576",
	"wsize=1048576",
	"mountproto=tcp",
	"mountport=2049",
	"proto=tcp",
	"port=2049",
	"nfsvers=3",
	"noacl",
	"noac",
	"lookupcache=none",
}

// ctoDroppedOptions are the caching knobs the cto profile removes from the base
// set. The NFS client's defaults then apply (ac, lookupcache=all, actimeo
// defaults), which is close-to-open semantics.
var ctoDroppedOptions = []string{"noac", "lookupcache=none"}

// nfsMountOptionsForProfile returns the mount options for a consistency profile,
// or nil for an empty/unknown profile so envd falls back to its built-in set. An
// unknown profile is deliberately not an error here: the API validates it, and
// the orchestrator must never fail a resume over a field it does not recognise.
func nfsMountOptionsForProfile(profile string) []string {
	switch profile {
	case VolumeConsistencyStrict:
		return append(slices.Clone(nfsBaseMountOptions), "nolock")
	case VolumeConsistencyCto:
		out := make([]string, 0, len(nfsBaseMountOptions))
		for _, opt := range nfsBaseMountOptions {
			if !slices.Contains(ctoDroppedOptions, opt) {
				out = append(out, opt)
			}
		}

		return append(out, "nolock")
	default:
		return nil
	}
}
