package engine

import (
	"fmt"
	"strconv"
	"sync"

	"github.com/coreos/go-iptables/iptables"
)

type HostPortManager struct {
	mu        sync.Mutex
	start     int
	end       int
	allocated map[string]int // key -> hostPort
}

type PortMapping struct {
	HostPort    int
	SandboxPort int
}

func NewHostPortManager(start, end int) *HostPortManager {
	return &HostPortManager{
		start:     start,
		end:       end,
		allocated: make(map[string]int),
	}
}

func (m *HostPortManager) Allocate(sandboxID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for port := m.start; port <= m.end; port++ {
		inUse := false
		for _, p := range m.allocated {
			if p == port {
				inUse = true
				break
			}
		}
		if !inUse {
			m.allocated[sandboxID] = port
			return port, nil
		}
	}
	return 0, fmt.Errorf("no available host port in range [%d, %d]", m.start, m.end)
}

func (m *HostPortManager) Release(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.allocated, key)
}

// AllocatePorts 为多个端口分配 HostPort
// 返回映射列表，如果失败自动回滚已分配的
func (m *HostPortManager) AllocatePorts(sandboxID string, ports []int) ([]PortMapping, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var mappings []PortMapping
	for _, port := range ports {
		key := sandboxID + "-" + strconv.Itoa(port)
		allocated := false
		for p := m.start; p <= m.end; p++ {
			inUse := false
			for _, used := range m.allocated {
				if used == p {
					inUse = true
					break
				}
			}
			if !inUse {
				m.allocated[key] = p
				mappings = append(mappings, PortMapping{HostPort: p, SandboxPort: port})
				allocated = true
				break
			}
		}
		if !allocated {
			// 回滚
			for _, mapp := range mappings {
				delete(m.allocated, sandboxID+"-"+strconv.Itoa(mapp.SandboxPort))
			}
			return nil, fmt.Errorf("no available host port for sandbox %s port %d", sandboxID, port)
		}
	}
	return mappings, nil
}

// ReleasePorts 释放所有端口
func (m *HostPortManager) ReleasePorts(sandboxID string, ports []int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, port := range ports {
		delete(m.allocated, sandboxID+"-"+strconv.Itoa(port))
	}
}

func SetupHostPortMapping(nodeIP string, hostPort int, sandboxIP string, sandboxPort int) error {
	tables, err := iptables.New()
	if err != nil {
		return fmt.Errorf("init iptables: %w", err)
	}

	// PREROUTING
	_ = tables.Append("nat", "PREROUTING",
		"-p", "tcp",
		"-d", nodeIP,
		"--dport", fmt.Sprintf("%d", hostPort),
		"-j", "DNAT",
		"--to-destination", fmt.Sprintf("%s:%d", sandboxIP, sandboxPort),
	)

	// OUTPUT（宿主机本地访问）
	_ = tables.Append("nat", "OUTPUT",
		"-p", "tcp",
		"-d", nodeIP,
		"--dport", fmt.Sprintf("%d", hostPort),
		"-j", "DNAT",
		"--to-destination", fmt.Sprintf("%s:%d", sandboxIP, sandboxPort),
	)

	// POSTROUTING
	_ = tables.Append("nat", "POSTROUTING",
		"-p", "tcp",
		"-d", sandboxIP,
		"--dport", fmt.Sprintf("%d", sandboxPort),
		"-j", "MASQUERADE",
	)

	// FORWARD 放行
	_ = tables.Append("filter", "FORWARD",
		"-p", "tcp",
		"-d", sandboxIP,
		"--dport", fmt.Sprintf("%d", sandboxPort),
		"-j", "ACCEPT",
	)
	_ = tables.Append("filter", "FORWARD",
		"-p", "tcp",
		"-s", sandboxIP,
		"--sport", fmt.Sprintf("%d", sandboxPort),
		"-j", "ACCEPT",
	)

	return nil
}

func CleanupHostPortMapping(nodeIP string, hostPort int, sandboxIP string, sandboxPort int) error {
	tables, err := iptables.New()
	if err != nil {
		return fmt.Errorf("init iptables: %w", err)
	}

	_ = tables.Delete("nat", "PREROUTING",
		"-p", "tcp", "-d", nodeIP, "--dport", fmt.Sprintf("%d", hostPort),
		"-j", "DNAT", "--to-destination", fmt.Sprintf("%s:%d", sandboxIP, sandboxPort),
	)
	_ = tables.Delete("nat", "OUTPUT",
		"-p", "tcp", "-d", nodeIP, "--dport", fmt.Sprintf("%d", hostPort),
		"-j", "DNAT", "--to-destination", fmt.Sprintf("%s:%d", sandboxIP, sandboxPort),
	)
	_ = tables.Delete("nat", "POSTROUTING",
		"-p", "tcp", "-d", sandboxIP, "--dport", fmt.Sprintf("%d", sandboxPort),
		"-j", "MASQUERADE",
	)
	_ = tables.Delete("filter", "FORWARD",
		"-p", "tcp", "-d", sandboxIP, "--dport", fmt.Sprintf("%d", sandboxPort),
		"-j", "ACCEPT",
	)
	_ = tables.Delete("filter", "FORWARD",
		"-p", "tcp", "-s", sandboxIP, "--sport", fmt.Sprintf("%d", sandboxPort),
		"-j", "ACCEPT",
	)

	return nil
}

