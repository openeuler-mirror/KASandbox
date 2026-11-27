package handlers

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/e2b-dev/infra/packages/api/internal/cfg"
	"github.com/e2b-dev/infra/packages/shared/pkg/consts"
	"github.com/e2b-dev/infra/packages/shared/pkg/servicediscovery"
)

// newStaticServiceDiscovery lists the orchestrators of a KASandbox systemd
// deployment. The node plane dials each of them, as it dials the pods under
// kubernetes. Each orchestrator also runs the template manager
// (ORCHESTRATOR_SERVICES=orchestrator,template-manager), so the builder plane
// lists the same instances; one that does not report the builder role is
// never selected for builds.
func newStaticServiceDiscovery(config cfg.Config) (serviceDiscovery, error) {
	items := parseStaticNodes(config.OrchestratorStaticNodes, consts.OrchestratorAPIPort)
	if len(items) == 0 {
		return serviceDiscovery{}, fmt.Errorf("static orchestrator discovery: ORCHESTRATOR_STATIC_NODES lists no node")
	}

	nodes := servicediscovery.NewStaticInstances(items)

	return serviceDiscovery{nodes: nodes, templateBuilders: nodes}, nil
}

// parseStaticNodes parses the KASandbox ORCHESTRATOR_STATIC_NODES list,
// "node1=10.0.0.1,node2=10.0.0.2:5009,node3", with the KASandbox rules: blank
// entries and entries without a node name are skipped, an entry without
// "=address" dials the node name, and an address without a valid port uses
// defaultPort. The node name is the instance's node; a build keeps the node it
// was placed on and finds the builder by it again. Repeated addresses are
// listed once.
func parseStaticNodes(raw string, defaultPort uint16) []servicediscovery.Instance {
	items := make([]servicediscovery.Instance, 0)
	seen := make(map[string]bool)
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		nodeID, addr, _ := strings.Cut(entry, "=")
		nodeID = strings.TrimSpace(nodeID)
		if nodeID == "" {
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
		if seen[workloadID] {
			continue
		}
		seen[workloadID] = true

		items = append(items, servicediscovery.Instance{
			WorkloadID: workloadID,
			NodeID:     nodeID,
			IPAddress:  host,
			Port:       port,
			Backend:    servicediscovery.BackendStatic,
		})
	}

	return items
}
