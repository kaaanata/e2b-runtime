package blockvol

// TODO(volumes/grpc): expose Service through the orchestrator VolumeService
// RPCs (CreateVolumeIncarnation, CloneRevision, SealIncarnation,
// DeleteRevision, DeleteIncarnation, ForgetVolumeOperation,
// ReconcileVolumes). The protobuf changes live in
// packages/shared/pkg/grpc/orchestrator and belong to the API/RPC track; this
// package must not edit the shared protobufs. Error mapping to implement
// there:
//
//	ErrInvalidID, ErrReclaimEvidenceMismatch      -> codes.InvalidArgument
//	ErrOperationConflict                          -> codes.AlreadyExists
//	ErrRevisionNotFound, ErrIncarnationNotFound   -> codes.NotFound
//	ErrResourceExhausted (CapacityError)          -> codes.ResourceExhausted
//	HeldByError, StillRunningError, ErrIncarnationHeld -> codes.FailedPrecondition
//	BlockedError                                  -> codes.FailedPrecondition (with holder details)
//	ErrHolderMismatch                             -> codes.PermissionDenied
//	*CommandError                                 -> codes.Internal (stderr in details, never in message)
//
// CloneOptions.PreserveFilesystemUUID must be derived server-side from the
// revision's recorded consistency and the fork mode, never taken verbatim
// from an external caller.
