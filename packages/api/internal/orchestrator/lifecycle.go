package orchestrator

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/api/internal/sandbox"
	"github.com/e2b-dev/infra/packages/shared/pkg/env"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	e2bcatalog "github.com/e2b-dev/infra/packages/shared/pkg/sandbox-catalog"
)

func (o *Orchestrator) addSandboxToRoutingTable(ctx context.Context, sbx sandbox.Sandbox) error {
	node := o.GetNode(sbx.ClusterID, sbx.NodeID)
	if node == nil {
		return fmt.Errorf("node '%s' not found", sbx.NodeID)
	}

	// For remote cluster nodes we are using gPRC metadata for routing registration instead
	if node.IsClusterNode() {
		return nil
	}

	nodeIP := routeNodeIPAddress(node, env.IsLocal())

	info := e2bcatalog.SandboxInfo{
		OrchestratorID: node.Metadata().ServiceInstanceID,
		OrchestratorIP: nodeIP,

		ExecutionID:      sbx.ExecutionID,
		StartedAt:        sbx.StartTime,
		MaxLengthInHours: int64(sbx.MaxInstanceLength / time.Hour),
	}

	lifetime := time.Duration(info.MaxLengthInHours) * time.Hour
	if sbx.EndTime.Equal(sandbox.NoRuntimeDeadline()) {
		lifetime = 0
	}

	return o.routingCatalog.StoreSandbox(ctx, sbx.SandboxID, &info, lifetime)
}

func (o *Orchestrator) addSandboxToRoutingTableOrLog(ctx context.Context, sandbox sandbox.Sandbox) {
	if err := o.addSandboxToRoutingTable(ctx, sandbox); err != nil {
		logger.L().Error(ctx, "error adding routing record to catalog", zap.Error(err), logger.WithSandboxID(sandbox.SandboxID))
	}
}
