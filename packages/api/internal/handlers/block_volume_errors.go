package handlers

// Machine-readable error codes (the Error schema's error_code) of block
// volume refusals, so clients can tell refusals with the same HTTP status
// apart without parsing messages.
const (
	// errCodeSharedIdentityRequired: forking a sandbox with block volumes
	// without allowSharedIdentity (409).
	errCodeSharedIdentityRequired = "block_volume_shared_identity_required"
)

const errMsgSharedIdentityRequired = "Forking a sandbox with block volumes resumes the same memory and filesystem identity in every fork; " +
	"set allowSharedIdentity to true to accept that"
