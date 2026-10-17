package handlers

// Machine-readable error codes (the Error schema's error_code) of block
// volume refusals, so clients can tell refusals with the same HTTP status
// apart without parsing messages.
const (
	// errCodeSharedIdentityRequired: forking a sandbox with block volumes
	// without allowSharedIdentity (409).
	errCodeSharedIdentityRequired = "block_volume_shared_identity_required"
	// errCodeVolumeAttached: the volume is mounted by a sandbox (409 on
	// delete, and on a second read-write mount of the same image).
	errCodeVolumeAttached = "volume_attached"
	// errCodeVolumeReferencedBySnapshot: a paused sandbox's snapshot needs a
	// revision of the volume (409 on delete).
	errCodeVolumeReferencedBySnapshot = "volume_referenced_by_snapshot"
	// errCodeVolumeCreating: the volume's first image does not exist yet
	// (409 on delete).
	errCodeVolumeCreating = "volume_creating"
	// errCodeTemplateLayoutMismatch: the template build has no workspace slot
	// that can hold a block mount (409 on sandbox create).
	errCodeTemplateLayoutMismatch = "template_layout_mismatch"
	// errCodeVolumeResizeInvalid: a resume asked to shrink a block volume, or
	// to resize the volumes of a sandbox that is running (409).
	errCodeVolumeResizeInvalid = "volume_resize_invalid"
)

const errMsgSharedIdentityRequired = "Forking a sandbox with block volumes resumes the same memory and filesystem identity in every fork; " +
	"set allowSharedIdentity to true to accept that"
