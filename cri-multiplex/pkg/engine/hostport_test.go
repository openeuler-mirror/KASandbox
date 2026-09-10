package engine

import (
	"fmt"
	"net"
	"strings"
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

	if _, err := m.AllocatePorts("sandbox-a", []ExposePortSpec{{GuestPort: 80}, {GuestPort: 443}}); err == nil {
		t.Fatal("expected partial allocation failure")
	}
	if len(m.allocated) != 0 {
		t.Fatalf("allocated entries after rollback = %v, want empty", m.allocated)
	}
}

func TestHostPortManagerAllocatePortsRelease(t *testing.T) {
	m := NewHostPortManager(32000, 32002)

	mappings, err := m.AllocatePorts("sandbox-a", []ExposePortSpec{{GuestPort: 80}, {GuestPort: 443}})
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

func TestParseExposePortsValid(t *testing.T) {
	specs, err := parseExposePorts("8080, 9090:29090 ,49983:38080-38082")
	if err != nil {
		t.Fatalf("parseExposePorts: %v", err)
	}
	if len(specs) != 3 {
		t.Fatalf("spec count = %d, want 3: %+v", len(specs), specs)
	}
	if specs[0].GuestPort != 8080 || specs[0].Kind != exposePortAuto {
		t.Fatalf("spec[0] mismatch: %+v", specs[0])
	}
	if specs[1].GuestPort != 9090 || specs[1].Kind != exposePortFixed || specs[1].HostPort != 29090 {
		t.Fatalf("spec[1] mismatch: %+v", specs[1])
	}
	if specs[2].GuestPort != 49983 || specs[2].Kind != exposePortRange || specs[2].HostStart != 38080 || specs[2].HostEnd != 38082 {
		t.Fatalf("spec[2] mismatch: %+v", specs[2])
	}

	// 写法②③的宿主端口不受池范围约束，但区间两端可以相同
	specs, err = parseExposePorts("8080:29090-29090")
	if err != nil || len(specs) != 1 || specs[0].Kind != exposePortRange || specs[0].HostStart != 29090 || specs[0].HostEnd != 29090 {
		t.Fatalf("single-port range mismatch: specs=%+v err=%v", specs, err)
	}
}

func TestParseExposePortsMalformed(t *testing.T) {
	cases := map[string]string{
		"non-numeric guest":          "abc",
		"empty entry":                "8080,,9090",
		"trailing comma":             "8080,",
		"blank string":               "",
		"guest port zero":            "0",
		"guest port too large":       "65536",
		"too many segments":          "8080:29090:1",
		"empty host part":            "8080:",
		"non-numeric host":           "8080:abc",
		"inverted range":             "8080:29091-29090",
		"range end non-numeric":      "8080:29090-abc",
		"duplicate guest":            "8080,8080",
		"duplicate guest cross-form": "8080,8080:29090",
		"fixed privileged port":      "8080:80",
		"fixed reserved 5008":        "8080:5008",
		"range start privileged":     "8080:1000-2000",
		"range covers reserved":      "8080:5000-5010",
	}
	for name, input := range cases {
		if _, err := parseExposePorts(input); err == nil {
			t.Errorf("%s: parseExposePorts(%q) should fail", name, input)
		}
	}
}

func TestAllocatePortsFixedAndRange(t *testing.T) {
	m := NewHostPortManager(34000, 34001)
	m.bindProbe = nil // 纯 map 判定，不做真实 bind 探测

	mappings, err := m.AllocatePorts("sandbox-a", []ExposePortSpec{
		{GuestPort: 8080, Kind: exposePortFixed, HostPort: 45678},                  // 写法②：池外指定
		{GuestPort: 9090, Kind: exposePortRange, HostStart: 36000, HostEnd: 36002}, // 写法③：区间首个
		{GuestPort: 49983}, // 写法①：池内自动
	})
	if err != nil {
		t.Fatalf("AllocatePorts: %v", err)
	}
	want := []PortMapping{{HostPort: 45678, SandboxPort: 8080}, {HostPort: 36000, SandboxPort: 9090}, {HostPort: 34000, SandboxPort: 49983}}
	if len(mappings) != len(want) {
		t.Fatalf("mappings = %+v, want %+v", mappings, want)
	}
	for i := range want {
		if mappings[i] != want[i] {
			t.Fatalf("mappings[%d] = %+v, want %+v", i, mappings[i], want[i])
		}
	}
}

func TestAllocatePortsAutoSkipsPinned(t *testing.T) {
	m := NewHostPortManager(34000, 34001)
	m.bindProbe = nil

	// 写法②钉住池内端口后，写法①的池内扫描应跳过它
	if _, err := m.AllocatePorts("sandbox-a", []ExposePortSpec{{GuestPort: 8080, Kind: exposePortFixed, HostPort: 34000}}); err != nil {
		t.Fatalf("AllocatePorts fixed: %v", err)
	}
	mappings, err := m.AllocatePorts("sandbox-b", []ExposePortSpec{{GuestPort: 80}})
	if err != nil {
		t.Fatalf("AllocatePorts auto: %v", err)
	}
	if mappings[0].HostPort != 34001 {
		t.Fatalf("auto allocation should skip pinned port, got %d", mappings[0].HostPort)
	}
}

func TestAllocatePortsFixedOccupiedByMapRollback(t *testing.T) {
	m := NewHostPortManager(34000, 34001)
	m.bindProbe = nil

	if _, err := m.AllocatePorts("sandbox-a", []ExposePortSpec{{GuestPort: 8080, Kind: exposePortFixed, HostPort: 45678}}); err != nil {
		t.Fatalf("AllocatePorts sandbox-a: %v", err)
	}
	// 第一个条目成功、第二个条目指定端口已被 map 占用 → 整批回滚
	if _, err := m.AllocatePorts("sandbox-b", []ExposePortSpec{
		{GuestPort: 80},
		{GuestPort: 8080, Kind: exposePortFixed, HostPort: 45678},
	}); err == nil {
		t.Fatal("expected occupied host port failure")
	}
	if len(m.allocated) != 1 {
		t.Fatalf("allocated entries after rollback = %v, want only sandbox-a's entry", m.allocated)
	}
}

func TestAllocatePortsFixedOccupiedByBindProbe(t *testing.T) {
	// 真实占用：宿主机其他进程（此处为测试 listener）已绑定目标端口
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	occupied := ln.Addr().(*net.TCPAddr).Port

	m := NewHostPortManager(34000, 34001) // 默认 defaultBindProbe
	if _, err := m.AllocatePorts("sandbox-a", []ExposePortSpec{{GuestPort: 8080, Kind: exposePortFixed, HostPort: occupied}}); err == nil {
		t.Fatal("expected bind probe failure on occupied port")
	}
	if len(m.allocated) != 0 {
		t.Fatalf("allocated entries after rollback = %v, want empty", m.allocated)
	}
}

func TestAllocatePortsRangeExhaustedRollback(t *testing.T) {
	m := NewHostPortManager(34000, 34001)
	m.bindProbe = nil

	// 占满区间 [36000, 36001]
	if _, err := m.AllocatePorts("sandbox-a", []ExposePortSpec{
		{GuestPort: 8000, Kind: exposePortFixed, HostPort: 36000},
		{GuestPort: 8001, Kind: exposePortFixed, HostPort: 36001},
	}); err != nil {
		t.Fatalf("AllocatePorts sandbox-a: %v", err)
	}
	if _, err := m.AllocatePorts("sandbox-b", []ExposePortSpec{
		{GuestPort: 80},
		{GuestPort: 8080, Kind: exposePortRange, HostStart: 36000, HostEnd: 36001},
	}); err == nil {
		t.Fatal("expected range exhaustion failure")
	}
	if len(m.allocated) != 2 {
		t.Fatalf("allocated entries after rollback = %v, want only sandbox-a's entries", m.allocated)
	}
}

func TestAllocatePortsPoolExhausted(t *testing.T) {
	m := NewHostPortManager(34000, 34000)
	m.bindProbe = nil

	// 池仅 1 个端口，两个写法①条目 → 第二个失败且整批回滚（行为变更：池耗尽即创建失败）
	if _, err := m.AllocatePorts("sandbox-a", []ExposePortSpec{{GuestPort: 80}, {GuestPort: 443}}); err == nil {
		t.Fatal("expected pool exhaustion failure")
	}
	if len(m.allocated) != 0 {
		t.Fatalf("allocated entries after rollback = %v, want empty", m.allocated)
	}
}

func TestBuildHostPortRestoreScript(t *testing.T) {
	mappings := []PortMapping{{HostPort: 20000, SandboxPort: 8080}, {HostPort: 20001, SandboxPort: 9090}}
	script := buildHostPortRestoreScript("192.0.2.10", mappings, "172.16.0.2", nil)

	if !strings.Contains(script, "*nat\n") || !strings.Contains(script, "*filter\n") {
		t.Fatalf("script missing table blocks:\n%s", script)
	}
	if got := strings.Count(script, "-A "); got != 10 { // 5 条/映射 × 2 映射
		t.Fatalf("script should contain 10 rules, got %d:\n%s", got, script)
	}
	if !strings.Contains(script, "cri-multiplex:hostport:192.0.2.10:20000:172.16.0.2:8080") {
		t.Fatalf("script missing owner comment:\n%s", script)
	}

	// 幂等：已存在 comment 的映射整体跳过
	existing := map[string]struct{}{hostPortRuleComment("192.0.2.10", 20000, "172.16.0.2", 8080): {}}
	script = buildHostPortRestoreScript("192.0.2.10", mappings, "172.16.0.2", existing)
	if strings.Count(script, "-A ") != 5 {
		t.Fatalf("script should contain 5 rules after dedup, got:\n%s", script)
	}

	// 全部已存在 → 空脚本
	existing[hostPortRuleComment("192.0.2.10", 20001, "172.16.0.2", 9090)] = struct{}{}
	if script := buildHostPortRestoreScript("192.0.2.10", mappings, "172.16.0.2", existing); script != "" {
		t.Fatalf("script should be empty when all rules exist, got:\n%s", script)
	}
}

func TestExistingHostPortComments(t *testing.T) {
	save := []byte(`# Generated by iptables-save
*nat
:PREROUTING ACCEPT [0:0]
-A PREROUTING -p tcp -d 192.0.2.10 --dport 20000 -m comment --comment cri-multiplex:hostport:192.0.2.10:20000:172.16.0.2:8080 -j DNAT --to-destination 172.16.0.2:8080
-A PREROUTING -p tcp -m comment --comment "cri-multiplex:other-rule" -j ACCEPT
COMMIT
*filter
-A FORWARD -p tcp -d 172.16.0.2 --dport 8080 -m comment --comment cri-multiplex:hostport:192.0.2.10:20000:172.16.0.2:8080 -j ACCEPT
COMMIT
`)
	comments := existingHostPortComments(save)
	if len(comments) != 1 {
		t.Fatalf("comments = %v, want exactly 1 hostport comment", comments)
	}
	if _, ok := comments["cri-multiplex:hostport:192.0.2.10:20000:172.16.0.2:8080"]; !ok {
		t.Fatalf("missing expected comment: %v", comments)
	}
}
