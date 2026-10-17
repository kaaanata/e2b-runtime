package blockvol

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	teamID = "0f7a1b2c-3d4e-4f60-8a7b-9c0d1e2f3a4b"
	volID  = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	incID  = "2b3c4d5e-6f70-4b8c-9d0e-1f2a3b4c5d6e"
	revID  = "3c4d5e6f-7081-4c9d-8e1f-2a3b4c5d6e7f"
	opID   = "4d5e6f70-8192-4dae-9f20-3b4c5d6e7f80"
)

func TestParseID(t *testing.T) {
	t.Parallel()

	valid := []string{teamID, "00000000-0000-4000-8000-000000000001"}
	for _, s := range valid {
		id, err := ParseID(s)
		require.NoError(t, err, s)
		assert.Equal(t, s, id.String())
		assert.False(t, id.IsZero())
	}

	invalid := map[string]string{
		"empty":       "",
		"uppercase":   "0F7A1B2C-3D4E-4F60-8A7B-9C0D1E2F3A4B",
		"braces":      "{0f7a1b2c-3d4e-4f60-8a7b-9c0d1e2f3a4b}",
		"urn":         "urn:uuid:0f7a1b2c-3d4e-4f60-8a7b-9c0d1e2f3a4b",
		"no dashes":   "0f7a1b2c3d4e4f608a7b9c0d1e2f3a4b",
		"nil uuid":    "00000000-0000-0000-0000-000000000000",
		"traversal":   "../../etc/passwd",
		"padded":      " 0f7a1b2c-3d4e-4f60-8a7b-9c0d1e2f3a4b",
		"slash":       "0f7a1b2c-3d4e-4f60-8a7b-9c0d1e2f3a4/",
		"too long":    "0f7a1b2c-3d4e-4f60-8a7b-9c0d1e2f3a4b0",
		"non hex":     "0f7a1b2c-3d4e-4f60-8a7b-9c0d1e2f3a4g",
		"newline":     "0f7a1b2c-3d4e-4f60-8a7b-9c0d1e2f3a4\n",
		"dot segment": ".f7a1b2c-3d4e-4f60-8a7b-9c0d1e2f3a4b",
	}
	for name, s := range invalid {
		_, err := ParseID(s)
		require.ErrorIs(t, err, ErrInvalidID, name)
	}
}

func TestIDJSONRoundTrip(t *testing.T) {
	t.Parallel()

	type doc struct {
		A ID `json:"a"`
		B ID `json:"b,omitzero"`
	}

	raw, err := json.Marshal(doc{A: MustID(teamID)})
	require.NoError(t, err)
	assert.JSONEq(t, `{"a":"`+teamID+`"}`, string(raw))

	var back doc
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Equal(t, teamID, back.A.String())
	assert.True(t, back.B.IsZero())

	var bad doc
	require.ErrorIs(t, json.Unmarshal([]byte(`{"a":"../x"}`), &bad), ErrInvalidID)

	_, err = json.Marshal(doc{})
	require.ErrorIs(t, err, ErrInvalidID, "a zero ID must not serialise as an empty string")
}

func TestLayoutPaths(t *testing.T) {
	t.Parallel()

	l, err := NewLayout("/srv/vol")
	require.NoError(t, err)

	vol := VolumeRef{Team: MustID(teamID), Volume: MustID(volID)}
	inc := IncarnationRef{VolumeRef: vol, Incarnation: MustID(incID)}
	rev := RevisionRef{VolumeRef: vol, Revision: MustID(revID)}

	assert.Equal(t, "/srv/vol", l.Root())
	assert.Equal(t, filepath.Join("/srv/vol/teams", teamID, volID, "inc", incID), l.IncarnationDir(inc))
	assert.Equal(t, filepath.Join("/srv/vol/teams", teamID, volID, "inc", incID, "workspace.ext4"), l.IncarnationImage(inc))
	assert.Equal(t, filepath.Join("/srv/vol/teams", teamID, volID, "rev", revID), l.RevisionDir(rev))
	assert.Equal(t, filepath.Join("/srv/vol/teams", teamID, volID, "rev", revID, "workspace.ext4"), l.RevisionImage(rev))
	assert.Equal(t, filepath.Join("/srv/vol/holders", incID+".json"), l.HolderPath(inc.Incarnation))
	assert.Equal(t, filepath.Join("/srv/vol/holders", incID+".lock"), l.HolderLockPath(inc.Incarnation))
	assert.Equal(t, filepath.Join("/srv/vol/staging", opID), l.StagingDir(MustID(opID)))
	assert.Equal(t, "/srv/vol/placeholders/34359738368.img", l.PlaceholderImage(32<<30))
}

func TestLayoutRootResolution(t *testing.T) {
	t.Setenv(RootEnv, "/from/env")

	l, err := NewLayout("")
	require.NoError(t, err)
	assert.Equal(t, "/from/env", l.Root())

	l, err = NewLayout("/explicit")
	require.NoError(t, err)
	assert.Equal(t, "/explicit", l.Root(), "explicit root wins over the environment")

	t.Setenv(RootEnv, "")

	l, err = NewLayout("")
	require.NoError(t, err)
	assert.Equal(t, DefaultRoot, l.Root())
}

func TestRefValidation(t *testing.T) {
	t.Parallel()

	vol := VolumeRef{Team: MustID(teamID), Volume: MustID(volID)}

	require.NoError(t, vol.validate())
	require.ErrorIs(t, VolumeRef{Team: MustID(teamID)}.validate(), ErrInvalidID)
	require.ErrorIs(t, IncarnationRef{VolumeRef: vol}.validate(), ErrInvalidID)
	require.ErrorIs(t, RevisionRef{VolumeRef: vol}.validate(), ErrInvalidID)
	require.NoError(t, IncarnationRef{VolumeRef: vol, Incarnation: MustID(incID)}.validate())
}
