package handlers

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/api/internal/cfg"
	"github.com/e2b-dev/infra/packages/shared/pkg/consts"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/servicediscovery"
)

// newStaticServiceDiscovery lists the orchestrators of a KASandbox systemd
// deployment. The node plane dials each of them, as it dials the pods under
// kubernetes. Each orchestrator also runs the template manager
// (ORCHESTRATOR_SERVICES=orchestrator,template-manager), so the builder plane
// lists the same instances; one that does not report the builder role is
// never selected for builds.
func newStaticServiceDiscovery(ctx context.Context, config cfg.Config) (serviceDiscovery, error) {
	items, ignored := parseStaticNodes(config.OrchestratorStaticNodes, consts.OrchestratorAPIPort)
	if len(ignored) > 0 {
		logger.L().Warn(ctx, "Ignoring ORCHESTRATOR_STATIC_NODES entries without a node name or naming a node or address listed before",
			zap.Strings("entries", ignored))
	}
	if len(items) == 0 {
		return serviceDiscovery{}, fmt.Errorf("static orchestrator discovery: ORCHESTRATOR_STATIC_NODES lists no node")
	}

	nodes := servicediscovery.NewStaticInstances(items)

	return serviceDiscovery{nodes: nodes, templateBuilders: nodes}, nil
}

// parseStaticNodes parses the KASandbox ORCHESTRATOR_STATIC_NODES list,
// "node1=10.0.0.1,node2=10.0.0.2:5009,node3", with the KASandbox rules: blank
// entries are skipped, an entry without "=address" dials the node name, and an
// address without a valid port uses defaultPort. The node name is the
// instance's node; a build keeps the node it was placed on and finds the
// builder by it again, so a name listed twice would leave that lookup to
// chance. Only the first entry for a node name or an address is listed; it
// returns the other non-blank entries it skipped, and those without a node
// name, as ignored.
func parseStaticNodes(raw string, defaultPort uint16) (items []servicediscovery.Instance, ignored []string) {
	items = make([]servicediscovery.Instance, 0)
	seenAddresses := make(map[string]bool)
	seenNodes := make(map[string]bool)
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		nodeID, addr, _ := strings.Cut(entry, "=")
		nodeID = strings.TrimSpace(nodeID)
		if nodeID == "" || seenNodes[nodeID] {
			ignored = append(ignored, entry)

			continue
		}

		addr = strings.TrimSpace(addr)
		if addr == "" {
			addr = nodeID
		}

		host, portStr, found := strings.Cut(addr, ":")
		port := defaultPort
		if found {
			if p, err := strconv.ParseUint(portStr, 10, 16); err == nil && p > 0 {
				port = uint16(p)
			}
		}

		workloadID := net.JoinHostPort(host, strconv.Itoa(int(port)))
		if seenAddresses[workloadID] {
			ignored = append(ignored, entry)

			continue
		}
		seenAddresses[workloadID] = true
		seenNodes[nodeID] = true

		items = append(items, servicediscovery.Instance{
			WorkloadID: workloadID,
			NodeID:     nodeID,
			IPAddress:  host,
			Port:       port,
			Backend:    servicediscovery.BackendStatic,
		})
	}

	return items, ignored
}
