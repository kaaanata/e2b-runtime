//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/launchdarkly/go-sdk-common/v3/ldvalue"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/credproxy"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/factories"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/tcpfirewall"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/version"
	"github.com/e2b-dev/infra/packages/shared/pkg/featureflags"
)

var commitSHA string

func main() {
	applyTestFlagOverrides()

	factories.Run(factories.Options{
		Version:       version.Version,
		CommitSHA:     commitSHA,
		EgressFactory: defaultEgressFactory,
	})
}

func applyTestFlagOverrides() {
	if mode := os.Getenv("TESTS_MEMFILE_DIFF_DEDUP_MODE"); mode != "" {
		featureflags.OverrideJSONFlag(featureflags.MemfileDiffDedupFlag, ldvalue.FromJSONMarshal(map[string]any{
			"enabled":    true,
			"bestEffort": mode == "best_effort",
			"directIO":   mode == "direct_io",
		}))
	}
	if os.Getenv("TESTS_DISABLE_MEMFD") == "true" {
		featureflags.OverrideBoolFlag(featureflags.UseMemFdFlag, false)
	}
}

func defaultEgressFactory(_ context.Context, deps *factories.Deps) (*factories.EgressSetup, error) {
	fw := tcpfirewall.New(
		deps.Logger,
		deps.Config.NetworkConfig,
		deps.Sandboxes,
		deps.MeterProvider,
		deps.FeatureFlags,
	)

	if root := os.Getenv("CREDPROXY_ROOT"); root != "" {
		if !filepath.IsAbs(root) {
			return nil, errors.New("CREDPROXY_ROOT must be absolute")
		}
		credentials, err := credproxy.New(root, os.Getenv("CREDPROXY_TEAM_ID"))
		if err != nil {
			return nil, err
		}
		fw.SetCredentialProxy(credentials)

		return &factories.EgressSetup{Proxy: fw, Start: func(startCtx context.Context) error {
			child, cancel := context.WithCancel(startCtx)
			defer cancel()
			results := make(chan error, 2)
			go func() { results <- credentials.ServeManagement(child) }()
			go func() { results <- fw.Start(child) }()
			first := <-results
			cancel()
			second := <-results

			return errors.Join(first, second)
		}, Close: fw.Close}, nil
	}

	return &factories.EgressSetup{
		Proxy: fw,
		Start: fw.Start,
		Close: fw.Close,
	}, nil
}
