package hostservice

import (
	"context"
	"fmt"
	"sync"
)

// CID (Context Identifier) is the addressing mechanism for AF_VSOCK:
//
//	CID 0: hypervisor (reserved)
//	CID 1: reserved
//	CID 2: host (the orchestrator itself)
//	CID 3+: one per sandbox
//
// VsockCIDBase is the first CID assignable to a sandbox. It is fixed at 3
// because 0/1/2 are reserved by the vsock protocol, so it is not configurable.
const VsockCIDBase int64 = 3

type CIDPool struct {
	mu      sync.Mutex
	max     int64
	used    map[int64]bool
	nextCID int64
}

func NewCIDPool(capacity int64) *CIDPool {
	if capacity <= 0 {
		capacity = 1000
	}
	return &CIDPool{
		max:     VsockCIDBase + capacity,
		used:    make(map[int64]bool),
		nextCID: VsockCIDBase,
	}
}

func (p *CIDPool) Allocate(ctx context.Context) (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	start := p.nextCID
	for i := int64(0); i < (p.max - VsockCIDBase); i++ {
		cid := start + i
		if cid >= p.max {
			cid = VsockCIDBase + (cid - p.max)
		}
		if !p.used[cid] {
			p.used[cid] = true
			p.nextCID = cid + 1
			if p.nextCID >= p.max {
				p.nextCID = VsockCIDBase
			}
			return cid, nil
		}
	}

	return 0, fmt.Errorf("CID pool exhausted (base=%d, max=%d, used=%d)", VsockCIDBase, p.max, len(p.used))
}

func (p *CIDPool) Release(cid int64) {
	if cid == 0 {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.used[cid] {
		delete(p.used, cid)
		p.nextCID = cid
	}
}

func (p *CIDPool) UsedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.used)
}
