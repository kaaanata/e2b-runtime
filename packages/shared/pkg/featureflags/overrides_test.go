package featureflags

import (
	"context"
	"testing"

	"github.com/launchdarkly/go-sdk-common/v3/ldvalue"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The offline store is shared by the whole package, so every test that applies
// an override restores the flag's code default afterwards.
func restoreIntFlag(t *testing.T, flag IntFlag) {
	t.Helper()
	t.Cleanup(func() {
		launchDarklyOfflineStore.Update(
			launchDarklyOfflineStore.Flag(flag.Key()).ValueForAll(ldvalue.Int(flag.Fallback())),
		)
	})
}

func restoreBoolFlag(t *testing.T, flag BoolFlag) {
	t.Helper()
	t.Cleanup(func() {
		launchDarklyOfflineStore.Update(
			launchDarklyOfflineStore.Flag(flag.Key()).VariationForAll(flag.Fallback()),
		)
	})
}

func restoreStringFlag(t *testing.T, flag StringFlag) {
	t.Helper()
	t.Cleanup(func() {
		launchDarklyOfflineStore.Update(
			launchDarklyOfflineStore.Flag(flag.Key()).ValueForAll(ldvalue.String(flag.Fallback())),
		)
	})
}

func restoreJSONFlag(t *testing.T, flag JSONFlag) {
	t.Helper()
	t.Cleanup(func() {
		launchDarklyOfflineStore.Update(
			launchDarklyOfflineStore.Flag(flag.Key()).ValueForAll(flag.Fallback()),
		)
	})
}

func newOfflineClient(t *testing.T) *Client {
	t.Helper()

	client, err := NewClient()
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, client.Close(context.WithoutCancel(t.Context())))
	})

	return client
}

//nolint:paralleltest // t.Setenv and the shared offline store
func TestOverrides_IntFlagWinsOverCodeDefault(t *testing.T) {
	restoreIntFlag(t, BestOfKMaxOvercommit)
	require.Equal(t, 400, BestOfKMaxOvercommit.Fallback(), "test assumes the code default")

	t.Setenv(FeatureFlagOverridesEnv, `{"best-of-k-max-overcommit": 200}`)
	client := newOfflineClient(t)

	assert.Equal(t, 200, client.IntFlag(t.Context(), BestOfKMaxOvercommit))
	// Other flags keep their defaults.
	assert.Equal(t, BestOfKSampleSize.Fallback(), client.IntFlag(t.Context(), BestOfKSampleSize))
}

//nolint:paralleltest // t.Setenv and the shared offline store
func TestOverrides_BoolFlag(t *testing.T) {
	restoreBoolFlag(t, EnableWriteThroughCacheFlag)
	require.False(t, EnableWriteThroughCacheFlag.Fallback(), "test assumes the code default")

	t.Setenv(FeatureFlagOverridesEnv, `{"write-to-cache-on-writes": true}`)
	client := newOfflineClient(t)

	assert.True(t, client.BoolFlag(t.Context(), EnableWriteThroughCacheFlag))
}

//nolint:paralleltest // t.Setenv and the shared offline store
func TestOverrides_StringAndJSONFlags(t *testing.T) {
	restoreStringFlag(t, ResumePrefetchSourceFlag)
	restoreJSONFlag(t, CleanNFSCache)

	t.Setenv(FeatureFlagOverridesEnv, `{"resume-prefetch-source": "none", "clean-nfs-cache": {"enabled": true, "limit": 3}}`)
	client := newOfflineClient(t)

	assert.Equal(t, "none", client.StringFlag(t.Context(), ResumePrefetchSourceFlag))
	assert.JSONEq(t, `{"enabled": true, "limit": 3}`, client.JSONFlag(t.Context(), CleanNFSCache).JSONString())
}

//nolint:paralleltest // t.Setenv and the shared offline store
func TestOverrides_UnknownKeyIsIgnored(t *testing.T) {
	restoreIntFlag(t, BestOfKMaxOvercommit)

	t.Setenv(FeatureFlagOverridesEnv, `{"no-such-flag": 1, "best-of-k-max-overcommit": 300}`)
	client := newOfflineClient(t)

	// The unknown key is skipped; the known one next to it is still applied.
	assert.Equal(t, 300, client.IntFlag(t.Context(), BestOfKMaxOvercommit))
}

//nolint:paralleltest // t.Setenv and the shared offline store
func TestOverrides_EmptyEnvChangesNothing(t *testing.T) {
	for _, raw := range []string{"", "   "} {
		t.Setenv(FeatureFlagOverridesEnv, raw)
		client := newOfflineClient(t)

		assert.Equal(t, BestOfKMaxOvercommit.Fallback(), client.IntFlag(t.Context(), BestOfKMaxOvercommit))
		assert.Equal(t, EnableWriteThroughCacheFlag.Fallback(), client.BoolFlag(t.Context(), EnableWriteThroughCacheFlag))
	}
}

//nolint:paralleltest // t.Setenv
func TestOverrides_InvalidInputFailsClientConstruction(t *testing.T) {
	restoreIntFlag(t, BestOfKMaxOvercommit)

	for name, raw := range map[string]string{
		"malformed json":             `{"best-of-k-max-overcommit": }`,
		"not an object":              `[1, 2]`,
		"scalar":                     `400`,
		"string for an int flag":     `{"best-of-k-max-overcommit": "400"}`,
		"float for an int flag":      `{"best-of-k-max-overcommit": 400.5}`,
		"int for a bool flag":        `{"write-to-cache-on-writes": 1}`,
		"bool for a string flag":     `{"resume-prefetch-source": true}`,
		"bad value after a good one": `{"best-of-k-max-overcommit": 200, "write-to-cache-on-writes": "yes"}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(FeatureFlagOverridesEnv, raw)

			client, err := NewClient()
			require.Error(t, err)
			assert.Nil(t, client)
			assert.Contains(t, err.Error(), FeatureFlagOverridesEnv)

			clientWithLevel, err := NewClientWithLogLevel(0)
			require.Error(t, err)
			assert.Nil(t, clientWithLevel)
		})
	}
}

//nolint:paralleltest // t.Setenv and the shared offline store
func TestOverrides_NewClientWithLogLevelAppliesThem(t *testing.T) {
	restoreIntFlag(t, BestOfKMaxOvercommit)

	t.Setenv(FeatureFlagOverridesEnv, `{"best-of-k-max-overcommit": 250}`)

	client, err := NewClientWithLogLevel(0)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, client.Close(context.WithoutCancel(t.Context())))
	})

	assert.Equal(t, 250, client.IntFlag(t.Context(), BestOfKMaxOvercommit))
}

func TestOverrides_EveryDeclaredFlagIsRegistered(t *testing.T) {
	t.Parallel()

	for _, key := range []string{
		BestOfKMaxOvercommit.Key(),
		EnableWriteThroughCacheFlag.Key(),
		ResumePrefetchSourceFlag.Key(),
		CleanNFSCache.Key(),
	} {
		_, ok := registeredFlags[key]
		assert.True(t, ok, key)
	}
	assert.Equal(t, intKind, registeredFlags[BestOfKMaxOvercommit.Key()])
	assert.Equal(t, boolKind, registeredFlags[EnableWriteThroughCacheFlag.Key()])
	assert.Equal(t, stringKind, registeredFlags[ResumePrefetchSourceFlag.Key()])
	assert.Equal(t, jsonKind, registeredFlags[CleanNFSCache.Key()])
}
