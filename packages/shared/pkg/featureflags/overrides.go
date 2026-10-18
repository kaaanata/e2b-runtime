package featureflags

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/launchdarkly/go-sdk-common/v3/ldvalue"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

// FeatureFlagOverridesEnv names the environment variable read when
// LAUNCH_DARKLY_API_KEY is empty: a JSON object {"flag-key": value, ...}
// applied to the offline store when a client is constructed, after the code
// defaults, so a deployment without LaunchDarkly can set a flag (for example
// {"best-of-k-max-overcommit": 400}) without rebuilding the service.
//
// Each value must have the type of the flag it sets: a bool for bool flags, an
// integer for int flags, a string for string flags, anything for JSON flags.
// Unknown keys are logged and ignored (a flag may exist only in a newer
// build); malformed JSON, a non-object, or a value of the wrong type is an
// error, so a misconfigured override never runs silently on the defaults.
// The variable is ignored, with a warning, when LAUNCH_DARKLY_API_KEY is set.
const FeatureFlagOverridesEnv = "FEATURE_FLAG_OVERRIDES"

// flagKind is the value type a flag was declared with; overrides are checked
// against it before they reach the offline store.
type flagKind int

const (
	boolKind flagKind = iota
	intKind
	stringKind
	jsonKind
)

func (k flagKind) String() string {
	switch k {
	case boolKind:
		return "bool"
	case intKind:
		return "int"
	case stringKind:
		return "string"
	case jsonKind:
		return "json"
	default:
		return "unknown"
	}
}

// registeredFlags records every flag declared through New*Flag, by key, so
// overrides can be checked against the declared type. It is written only
// during package initialization.
var registeredFlags = map[string]flagKind{}

func registerFlag(name string, kind flagKind) {
	registeredFlags[name] = kind
}

// applyEnvOverrides reads FeatureFlagOverridesEnv and applies it to the offline
// store. Called from the constructors that use the offline store, before the
// LaunchDarkly client is created from it.
func applyEnvOverrides(ctx context.Context) error {
	return applyOverrides(ctx, os.Getenv(FeatureFlagOverridesEnv))
}

func applyOverrides(ctx context.Context, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	var overrides ldvalue.Value
	if err := json.Unmarshal([]byte(raw), &overrides); err != nil {
		return fmt.Errorf("%s is not valid JSON: %w", FeatureFlagOverridesEnv, err)
	}
	if overrides.Type() != ldvalue.ObjectType {
		return fmt.Errorf("%s must be a JSON object of {\"flag-key\": value}, got %s", FeatureFlagOverridesEnv, overrides.Type())
	}

	keys := overrides.Keys(nil)
	slices.Sort(keys)

	for _, key := range keys {
		value := overrides.GetByKey(key)

		kind, known := registeredFlags[key]
		if !known {
			logger.L().Warn(ctx, "ignoring feature flag override for an unknown flag",
				zap.String("env", FeatureFlagOverridesEnv),
				zap.String("flag", key),
			)

			continue
		}

		if err := checkOverrideType(kind, value); err != nil {
			return fmt.Errorf("%s: flag %q: %w", FeatureFlagOverridesEnv, key, err)
		}

		launchDarklyOfflineStore.Update(launchDarklyOfflineStore.Flag(key).ValueForAll(value))

		logger.L().Info(ctx, "feature flag override applied",
			zap.String("env", FeatureFlagOverridesEnv),
			zap.String("flag", key),
			zap.String("value", value.JSONString()),
		)
	}

	return nil
}

func checkOverrideType(kind flagKind, value ldvalue.Value) error {
	switch kind {
	case boolKind:
		if value.Type() != ldvalue.BoolType {
			return fmt.Errorf("expected a bool, got %s", value.JSONString())
		}
	case intKind:
		if value.Type() != ldvalue.NumberType || !value.IsInt() {
			return fmt.Errorf("expected an integer, got %s", value.JSONString())
		}
	case stringKind:
		if value.Type() != ldvalue.StringType {
			return fmt.Errorf("expected a string, got %s", value.JSONString())
		}
	case jsonKind:
		// Any JSON value is a valid JSON flag value.
	default:
		return fmt.Errorf("unknown flag kind %s", kind)
	}

	return nil
}

// warnIgnoredOverrides logs when FeatureFlagOverridesEnv is set alongside a
// LaunchDarkly API key: LaunchDarkly is the source of truth then and the
// overrides are not applied.
func warnIgnoredOverrides(ctx context.Context) {
	if strings.TrimSpace(os.Getenv(FeatureFlagOverridesEnv)) == "" {
		return
	}

	logger.L().Warn(ctx, "ignoring feature flag overrides because LaunchDarkly is configured",
		zap.String("env", FeatureFlagOverridesEnv),
	)
}
