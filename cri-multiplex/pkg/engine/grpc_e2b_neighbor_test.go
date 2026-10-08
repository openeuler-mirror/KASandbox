package engine

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestGRPCE2BRepairsNeighborBeforeCreate(t *testing.T) {
	for _, pooled := range []bool{false, true} {
		name := "direct"
		if pooled {
			name = "pool"
		}
		t.Run(name, func(t *testing.T) {
			client := &fakeSandboxServiceClient{}
			e := newTestGRPCE2BEngine(client)
			rec := &CNIRecord{SandboxID: "uid-a", NetNSName: "test-ns", NetNSPath: "/var/run/netns/test-ns", IfName: "eth0", PodIP: "192.0.2.2"}
			cni := &fakeCNIManager{addRecord: rec}
			e.cniConfig.Enabled, e.cniManager = true, cni
			wantAdds := 1
			if pooled {
				rec.SandboxID = "pool12345678"
				e.cniPoolReady = make(chan *CNIRecord, 1)
				e.cniPoolReady <- rec
				e.markPendingNetNS(rec.NetNSName)
				wantAdds = 0
			}
			calls := 0
			e.repairPodNeighbor = func(ctx context.Context, path, iface, ip string) (bool, error) {
				calls++
				if cni.addCalls != wantAdds || client.createCalls != 0 || !e.isPendingNetNS(rec.NetNSName) {
					t.Fatalf("repair order: add=%d create=%d pending=%v", cni.addCalls, client.createCalls, e.isPendingNetNS(rec.NetNSName))
				}
				if path != rec.NetNSPath || iface != rec.IfName || ip != rec.PodIP {
					t.Fatalf("wrong repair target: %s %s %s", path, iface, ip)
				}
				return true, nil
			}
			if _, err := e.RunPodSandbox(context.Background(), e2bRunReq("uid-a")); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || client.createCalls != 1 || cni.delCalls != 0 || e.isPendingNetNS(rec.NetNSName) {
				t.Fatalf("calls repair=%d create=%d del=%d or pending leaked", calls, client.createCalls, cni.delCalls)
			}
		})
	}
}

func TestGRPCE2BNeighborFailureRollsBackWithoutCanceledContext(t *testing.T) {
	for _, pooled := range []bool{false, true} {
		name := "direct"
		if pooled {
			name = "pool"
		}
		t.Run(name, func(t *testing.T) {
			client := &fakeSandboxServiceClient{}
			e := newTestGRPCE2BEngine(client)
			rec := &CNIRecord{SandboxID: "uid-a", NetNSName: "test-ns", NetNSPath: "/var/run/netns/test-ns", IfName: "eth0", PodIP: "192.0.2.2"}
			cni := &fakeCNIManager{addRecord: rec}
			e.cniConfig.Enabled, e.cniManager = true, cni
			if pooled {
				rec.SandboxID = "pool12345678"
				e.cniPoolReady = make(chan *CNIRecord, 1)
				e.cniPoolReady <- rec
				e.markPendingNetNS(rec.NetNSName)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			e.repairPodNeighbor = func(context.Context, string, string, string) (bool, error) {
				cancel()
				return false, errors.New("netlink failure")
			}
			if _, err := e.RunPodSandbox(ctx, e2bRunReq("uid-a")); status.Code(err) != codes.Unavailable {
				t.Fatalf("error=%v, want Unavailable", err)
			}
			if client.createCalls != 0 || cni.delCalls != 1 || cni.delIDs[0] != rec.SandboxID || cni.delCtxErr != nil {
				t.Fatalf("rollback: create=%d del=%d IDs=%v context=%v", client.createCalls, cni.delCalls, cni.delIDs, cni.delCtxErr)
			}
			if _, ok := e.tracker.Get("uid-a"); ok || e.isPendingNetNS(rec.NetNSName) {
				t.Fatal("failed sandbox left tracked or pending")
			}
		})
	}
}

func TestGRPCE2BWithoutCNISkipsNeighborRepair(t *testing.T) {
	e := newTestGRPCE2BEngine(&fakeSandboxServiceClient{})
	e.repairPodNeighbor = func(context.Context, string, string, string) (bool, error) {
		t.Fatal("repair called without CNI")
		return false, nil
	}
	if _, err := e.RunPodSandbox(context.Background(), e2bRunReq("uid-a")); err != nil {
		t.Fatal(err)
	}
}

// AdminCreate must use the same repair ordering and failure cleanup as CRI.
func TestAdminCreateNeighborRepair(t *testing.T) {
	for _, pooled := range []bool{false, true} {
		for _, fail := range []bool{false, true} {
			name := "direct"
			if pooled {
				name = "pool"
			}
			if fail {
				name += "-failure"
			}
			t.Run(name, func(t *testing.T) {
				client := &fakeSandboxServiceClient{}
				e := newTestGRPCE2BEngine(client)
				rec := &CNIRecord{SandboxID: "sbx-1", NetNSName: "admin-ns", NetNSPath: "/var/run/netns/admin-ns", IfName: "eth0", PodIP: "192.0.2.3"}
				cni := &fakeCNIManager{addRecord: rec}
				e.cniConfig.Enabled, e.cniManager = true, cni
				if pooled {
					rec.SandboxID = "pool12345678"
					e.cniPoolReady = make(chan *CNIRecord, 1)
					e.cniPoolReady <- rec
					e.markPendingNetNS(rec.NetNSName)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				calls := 0
				e.repairPodNeighbor = func(_ context.Context, path, iface, ip string) (bool, error) {
					calls++
					if client.createCalls != 0 || !e.isPendingNetNS(rec.NetNSName) {
						t.Fatal("repair must precede orchestrator create while namespace remains pending")
					}
					if path != rec.NetNSPath || iface != rec.IfName || ip != rec.PodIP {
						t.Fatal("repair received the wrong CNI record")
					}
					if fail {
						cancel()
						return false, errors.New("repair failed")
					}
					return true, nil
				}
				_, err := e.AdminCreate(ctx, adminCreateReq("sbx-1"))
				if calls != 1 || e.isPendingNetNS(rec.NetNSName) {
					t.Fatalf("repair calls=%d or pending namespace leaked", calls)
				}
				if fail {
					if status.Code(err) != codes.Unavailable || client.createCalls != 0 || cni.delCalls != 1 || cni.delCtxErr != nil {
						t.Fatalf("rollback error=%v create=%d del=%d context=%v", err, client.createCalls, cni.delCalls, cni.delCtxErr)
					}
				} else if err != nil || client.createCalls != 1 || cni.delCalls != 0 {
					t.Fatalf("create error=%v create=%d del=%d", err, client.createCalls, cni.delCalls)
				}
			})
		}
	}
}
