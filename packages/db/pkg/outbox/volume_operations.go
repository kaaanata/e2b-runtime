package outbox

import (
	"fmt"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/e2b-dev/infra/packages/shared/pkg/sharedriver"
)

// Block volume operation kinds, as stored in volume_operations.kind.
const (
	VolumeOperationCreate    = "create"
	VolumeOperationClone     = "clone"
	VolumeOperationSeal      = "seal"
	VolumeOperationDelete    = "delete"
	VolumeOperationReconcile = "reconcile"
)

// Block volume operation states, as stored in volume_operations.state.
const (
	VolumeOperationPending   = "pending"
	VolumeOperationRunning   = "running"
	VolumeOperationUnknown   = "unknown"
	VolumeOperationSucceeded = "succeeded"
	VolumeOperationFailed    = "failed"
)

// Clone modes of VolumeCloneParams.Mode.
const (
	VolumeCloneColdBoot      = "cold-boot"
	VolumeCloneMemoryRestore = "memory-restore"
)

// VolumeOperationParams is the volume_operations.params document: everything
// a worker needs to (re)issue the operation. Identifiers of what the
// operation creates are allocated before the first attempt, so every retry
// sends the orchestrator the same request.
type VolumeOperationParams struct {
	ClusterID uuid.UUID `json:"cluster_id"`
	NodeID    string    `json:"node_id"`
	TeamID    uuid.UUID `json:"team_id"`
	VolumeID  uuid.UUID `json:"volume_id"`

	Create *VolumeCreateParams `json:"create,omitempty"`
	Clone  *VolumeCloneParams  `json:"clone,omitempty"`
	Seal   *VolumeSealParams   `json:"seal,omitempty"`
	// Delete scopes a "delete" operation. Absent deletes the whole volume.
	Delete *VolumeDeleteParams `json:"delete,omitempty"`
}

// VolumeDeleteParams narrows a "delete" operation to some incarnations of a
// volume that stays: the images a failed fork cloned and nothing will bind.
// The volume itself, its other incarnations and its revisions are kept.
type VolumeDeleteParams struct {
	IncarnationIDs []uuid.UUID `json:"incarnation_ids,omitempty"`
}

// Scoped reports whether the delete removes only some incarnations.
func (p *VolumeDeleteParams) Scoped() bool {
	return p != nil && len(p.IncarnationIDs) > 0
}

// VolumeCreateParams creates a volume's head incarnation and revision 0.
type VolumeCreateParams struct {
	IncarnationID uuid.UUID `json:"incarnation_id"`
	RevisionID    uuid.UUID `json:"revision_id"`
	CapacityBytes int64     `json:"capacity_bytes"`
}

// VolumeCloneParams derives incarnation IncarnationID of VolumeID from a
// revision of SourceVolumeID (the same volume for a sandbox fork, another
// one for a volume clone).
type VolumeCloneParams struct {
	SourceVolumeID    uuid.UUID `json:"source_volume_id"`
	SourceRevisionID  uuid.UUID `json:"source_revision_id"`
	SourceConsistency string    `json:"source_consistency"`
	Mode              string    `json:"mode"`
	IncarnationID     uuid.UUID `json:"incarnation_id"`
	CapacityBytes     int64     `json:"capacity_bytes"`
	// NewVolume marks a clone into a volume created for it: the incarnation
	// becomes that volume's head and the volume becomes ready.
	NewVolume bool `json:"new_volume,omitempty"`
}

// VolumeSealParams seals incarnation IncarnationID into revision RevisionID.
type VolumeSealParams struct {
	IncarnationID uuid.UUID `json:"incarnation_id"`
	RevisionID    uuid.UUID `json:"revision_id"`
	// ExecutionID is the holder the seal asserts; empty for an offline seal.
	ExecutionID string `json:"execution_id,omitempty"`
	Consistency string `json:"consistency"`
}

// The River jobs carry only the operation id: business state lives in
// volume_operations, which outlives River's job retention.

// VolumeCreate works a "create" volume operation.
type VolumeCreate struct {
	OperationID uuid.UUID `json:"operation_id"`
}

func (VolumeCreate) Kind() string                 { return "volume.create" }
func (VolumeCreate) InsertOpts() river.InsertOpts { return volumeInsertOpts() }
func (a VolumeCreate) Operation() uuid.UUID       { return a.OperationID }

// VolumeClone works a "clone" volume operation.
type VolumeClone struct {
	OperationID uuid.UUID `json:"operation_id"`
}

func (VolumeClone) Kind() string                 { return "volume.clone" }
func (VolumeClone) InsertOpts() river.InsertOpts { return volumeInsertOpts() }
func (a VolumeClone) Operation() uuid.UUID       { return a.OperationID }

// VolumeSeal works a "seal" volume operation.
type VolumeSeal struct {
	OperationID uuid.UUID `json:"operation_id"`
}

func (VolumeSeal) Kind() string                 { return "volume.seal" }
func (VolumeSeal) InsertOpts() river.InsertOpts { return volumeInsertOpts() }
func (a VolumeSeal) Operation() uuid.UUID       { return a.OperationID }

// VolumeDelete works a "delete" volume operation.
type VolumeDelete struct {
	OperationID uuid.UUID `json:"operation_id"`
}

func (VolumeDelete) Kind() string                 { return "volume.delete" }
func (VolumeDelete) InsertOpts() river.InsertOpts { return volumeInsertOpts() }
func (a VolumeDelete) Operation() uuid.UUID       { return a.OperationID }

// VolumeReconcile works a "reconcile" volume operation.
type VolumeReconcile struct {
	OperationID uuid.UUID `json:"operation_id"`
}

func (VolumeReconcile) Kind() string                 { return "volume.reconcile" }
func (VolumeReconcile) InsertOpts() river.InsertOpts { return volumeInsertOpts() }
func (a VolumeReconcile) Operation() uuid.UUID       { return a.OperationID }

// VolumeJobMaxAttempts bounds how long an operation is retried. At the
// River outbox's 15-minute retry cap this is about a day; the worker marks
// the operation failed on the last attempt.
const VolumeJobMaxAttempts = 100

// VolumeJobKinds lists the River kinds of block volume jobs.
func VolumeJobKinds() []string {
	return []string{
		VolumeCreate{}.Kind(), VolumeClone{}.Kind(), VolumeSeal{}.Kind(),
		VolumeDelete{}.Kind(), VolumeReconcile{}.Kind(),
	}
}

// VolumeJobArgs returns the River job working an operation of the given kind.
func VolumeJobArgs(kind string, operationID uuid.UUID) (river.JobArgs, error) {
	switch kind {
	case VolumeOperationCreate:
		return VolumeCreate{OperationID: operationID}, nil
	case VolumeOperationClone:
		return VolumeClone{OperationID: operationID}, nil
	case VolumeOperationSeal:
		return VolumeSeal{OperationID: operationID}, nil
	case VolumeOperationDelete:
		return VolumeDelete{OperationID: operationID}, nil
	case VolumeOperationReconcile:
		return VolumeReconcile{OperationID: operationID}, nil
	default:
		return nil, fmt.Errorf("unknown volume operation kind %q", kind)
	}
}

// volumeInsertOpts makes a repeat enqueue of the same operation a no-op while
// its job is open.
func volumeInsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       river.QueueDefault,
		Priority:    sharedriver.PriorityNormal,
		MaxAttempts: VolumeJobMaxAttempts,
		UniqueOpts: river.UniqueOpts{
			ByArgs: true,
			ByState: []rivertype.JobState{
				rivertype.JobStateAvailable,
				rivertype.JobStatePending,
				rivertype.JobStateRetryable,
				rivertype.JobStateRunning,
				rivertype.JobStateScheduled,
			},
		},
	}
}
