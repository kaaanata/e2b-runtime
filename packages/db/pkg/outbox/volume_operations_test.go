package outbox_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/db/pkg/outbox"
)

func TestVolumeJobArgs(t *testing.T) {
	t.Parallel()

	op := uuid.New()
	for kind, want := range map[string]string{
		outbox.VolumeOperationCreate:    "volume.create",
		outbox.VolumeOperationClone:     "volume.clone",
		outbox.VolumeOperationSeal:      "volume.seal",
		outbox.VolumeOperationDelete:    "volume.delete",
		outbox.VolumeOperationReconcile: "volume.reconcile",
	} {
		args, err := outbox.VolumeJobArgs(kind, op)
		require.NoError(t, err)
		assert.Equal(t, want, args.Kind())
		assert.Contains(t, outbox.VolumeJobKinds(), want)
	}

	_, err := outbox.VolumeJobArgs("format", op)
	require.Error(t, err)
}

func TestVolumeJobIsEnqueuedOncePerOperationWhileOpen(t *testing.T) {
	t.Parallel()

	client, _ := newInsertOnlyClient(t)
	op := uuid.New()

	first, err := client.Insert(t.Context(), outbox.VolumeCreate{OperationID: op}, nil)
	require.NoError(t, err)
	require.False(t, first.UniqueSkippedAsDuplicate)
	assert.Equal(t, outbox.VolumeJobMaxAttempts, first.Job.MaxAttempts)

	repeat, err := client.Insert(t.Context(), outbox.VolumeCreate{OperationID: op}, nil)
	require.NoError(t, err)
	require.True(t, repeat.UniqueSkippedAsDuplicate)

	other, err := client.Insert(t.Context(), outbox.VolumeDelete{OperationID: op}, nil)
	require.NoError(t, err)
	require.False(t, other.UniqueSkippedAsDuplicate, "uniqueness is per kind")
}
