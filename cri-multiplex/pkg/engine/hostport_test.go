package engine

import (
	"fmt"
	"sync"
	"testing"
)

func TestHostPortManagerAllocateRelease(t *testing.T) {
	m := NewHostPortManager(30000, 30001)

	p1, err := m.Allocate("sandbox-a")
	if err != nil {
		t.Fatalf("Allocate sandbox-a: %v", err)
	}
	if p1 != 30000 {
		t.Fatalf("first port = %d, want 30000", p1)
	}

	p2, err := m.Allocate("sandbox-b")
	if err != nil {
		t.Fatalf("Allocate sandbox-b: %v", err)
	}
	if p2 != 30001 {
		t.Fatalf("second port = %d, want 30001", p2)
	}

	if _, err := m.Allocate("sandbox-c"); err == nil {
		t.Fatal("expected exhaustion error")
	}

	m.Release("sandbox-a")
	p3, err := m.Allocate("sandbox-c")
	if err != nil {
		t.Fatalf("Allocate sandbox-c after release: %v", err)
	}
	if p3 != 30000 {
		t.Fatalf("reused port = %d, want 30000", p3)
	}
}

func TestHostPortManagerAllocatePortsRollback(t *testing.T) {
	m := NewHostPortManager(31000, 31000)

	if _, err := m.AllocatePorts("sandbox-a", []int{80, 443}); err == nil {
		t.Fatal("expected partial allocation failure")
	}
	if len(m.allocated) != 0 {
		t.Fatalf("allocated entries after rollback = %v, want empty", m.allocated)
	}
}

func TestHostPortManagerAllocatePortsRelease(t *testing.T) {
	m := NewHostPortManager(32000, 32002)

	mappings, err := m.AllocatePorts("sandbox-a", []int{80, 443})
	if err != nil {
		t.Fatalf("AllocatePorts: %v", err)
	}
	if len(mappings) != 2 {
		t.Fatalf("mapping count = %d, want 2", len(mappings))
	}
	if mappings[0].HostPort == mappings[1].HostPort {
		t.Fatalf("host ports should be unique: %+v", mappings)
	}

	m.ReleasePorts("sandbox-a", []int{80, 443})
	if len(m.allocated) != 0 {
		t.Fatalf("allocated entries after ReleasePorts = %v, want empty", m.allocated)
	}
}

func TestHostPortManagerConcurrentAllocateUnique(t *testing.T) {
	m := NewHostPortManager(33000, 33099)
	const workers = 50

	var wg sync.WaitGroup
	ports := make(chan int, workers)
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p, err := m.Allocate(fmt.Sprintf("sandbox-%d", i))
			if err != nil {
				errs <- err
				return
			}
			ports <- p
		}(i)
	}
	wg.Wait()
	close(ports)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("Allocate returned error: %v", err)
		}
	}
	seen := map[int]bool{}
	for p := range ports {
		if seen[p] {
			t.Fatalf("duplicate port allocated: %d", p)
		}
		seen[p] = true
	}
	if len(seen) != workers {
		t.Fatalf("allocated %d ports, want %d", len(seen), workers)
	}
}
