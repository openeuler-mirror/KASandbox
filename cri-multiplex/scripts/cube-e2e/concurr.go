package main

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	adminv2 "github.com/cri-multiplex/pkg/admin/v2"
)

type createSample struct {
	idx  int
	took time.Duration
	id   string
	ip   string
	err  error
}

// runConcurr 冷启动对齐 cube-bench：-c 并发 worker、-n 总请求（create-only 攒着，结束后清理）。
// concurrency<=0 时退化为一次性打满 n（旧行为）。
func runConcurr(criSock, cubeAPI, template string, total, concurrency int, stepTimeout time.Duration) error {
	if total < 1 {
		total = 1
	}
	if concurrency < 1 {
		concurrency = total
	}
	if concurrency > total {
		concurrency = total
	}
	ctx := context.Background()
	anns, err := loadColdAnnotations(ctx, cubeAPI, template)
	if err != nil {
		return err
	}
	fmt.Printf("concurr c=%d n=%d template=%s image=%s cpu=%s mem=%s\n",
		concurrency, total, anns["cube.dev/template-id"], anns["cube.dev/image"], anns["cube.dev/vcpu"], anns["cube.dev/ram-mb"])

	criConn, err := grpc.NewClient(criSock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer criConn.Close()
	rt := runtime.NewRuntimeServiceClient(criConn)

	jobs := make(chan int, total)
	for i := 0; i < total; i++ {
		jobs <- i
	}
	close(jobs)

	samples := make([]createSample, total)
	var wg sync.WaitGroup
	var okCount atomic.Int64
	wallStart := time.Now()
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				uid := fmt.Sprintf("cube-c-%d-%d", time.Now().UnixNano(), i)
				t0 := time.Now()
				cctx, cancel := context.WithTimeout(ctx, stepTimeout)
				resp, err := rt.RunPodSandbox(cctx, &runtime.RunPodSandboxRequest{
					RuntimeHandler: "cube",
					Config: &runtime.PodSandboxConfig{
						Metadata:    &runtime.PodSandboxMetadata{Name: "cube-c", Uid: uid, Namespace: "default", Attempt: 1},
						Annotations: anns,
						Labels:      map[string]string{"app": "cube-concurr"},
					},
				})
				cancel()
				s := createSample{idx: i, took: time.Since(t0), err: err}
				if err == nil {
					s.id = resp.GetPodSandboxId()
					okCount.Add(1)
					if st, stErr := rt.PodSandboxStatus(context.Background(), &runtime.PodSandboxStatusRequest{PodSandboxId: s.id}); stErr == nil {
						s.ip = st.GetStatus().GetNetwork().GetIp()
					}
				}
				samples[i] = s
				if err != nil {
					fmt.Printf("create[%d] FAIL took=%s err=%v\n", i, s.took.Round(time.Millisecond), err)
				} else {
					fmt.Printf("create[%d] OK took=%s id=%s ip=%s\n", i, s.took.Round(time.Millisecond), s.id, s.ip)
				}
			}
		}()
	}
	wg.Wait()
	wall := time.Since(wallStart)

	// cleanup all successes (create-only during the tier, like cube-bench §3.2).
	// Serial-ish with retry: cubelet destroy flow is rate limited (130513),
	// concurrent destroys leak taps (192.168.x.x) and slow later creates.
	var cleanFail int
	cleaned := cleanupAllThrottled(rt, samples, cleanupConcurrency)
	for _, c := range cleaned {
		if c.err != nil {
			cleanFail++
			fmt.Printf("cleanup FAIL id=%s err=%v\n", c.id, c.err)
		}
	}
	if left := staleTapCount(); left > 0 {
		fmt.Printf("stale taps after cleanup: %d\n", left)
	}

	ok := int(okCount.Load())
	fail := total - ok
	var okTook []time.Duration
	for _, s := range samples {
		if s.err == nil {
			okTook = append(okTook, s.took)
		}
	}
	sort.Slice(okTook, func(i, j int) bool { return okTook[i] < okTook[j] })

	fmt.Println()
	fmt.Printf("==== concurr create summary ====\n")
	fmt.Printf("c=%d n=%d ok=%d fail=%d cleanup_fail=%d wall=%s\n", concurrency, total, ok, fail, cleanFail, wall.Round(time.Millisecond))
	if len(okTook) > 0 {
		var sum time.Duration
		for _, d := range okTook {
			sum += d
		}
		avg := sum / time.Duration(len(okTook))
		amort := wall / time.Duration(len(okTook))
		tps := float64(len(okTook)) / wall.Seconds()
		fmt.Printf("create latency: avg=%s min=%s p50=%s p95=%s max=%s\n",
			avg.Round(time.Millisecond),
			okTook[0].Round(time.Millisecond),
			pct(okTook, 50).Round(time.Millisecond),
			pct(okTook, 95).Round(time.Millisecond),
			okTook[len(okTook)-1].Round(time.Millisecond),
		)
		fmt.Printf("per-sandbox wall amortized: %s\n", amort.Round(time.Millisecond))
		fmt.Printf("throughput: %.1f /s\n", tps)
		fmt.Printf("[CRIMetric] op=create c=%d n=%d ok=%d fail=%d wall_ms=%d avg_ms=%d min_ms=%d p50_ms=%d p95_ms=%d max_ms=%d amort_ms=%d throughput_per_s=%.1f\n",
			concurrency, total, ok, fail,
			wall.Milliseconds(), avg.Milliseconds(), okTook[0].Milliseconds(),
			pct(okTook, 50).Milliseconds(), pct(okTook, 95).Milliseconds(),
			okTook[len(okTook)-1].Milliseconds(), amort.Milliseconds(), tps)
	}
	if fail > 0 {
		return fmt.Errorf("concurr create: %d/%d failed", fail, total)
	}
	if cleanFail > 0 {
		return fmt.Errorf("concurr create ok but cleanup failed for %d", cleanFail)
	}
	return nil
}

// cleanupConcurrency bounds in-flight destroys; cubelet rate limit is tight.
const cleanupConcurrency = 2
const cleanupRetry = 8
const cleanupRetryBackoff = 1500 * time.Millisecond

type cleanupResult struct {
	id  string
	err error
}

// cleanupAllThrottled destroys sandboxes with limited concurrency and retries,
// so cubelet's destroy flow limiter (ret 130513) is not overwhelmed; leftover
// taps would otherwise slow later creates via netlink dumps.
func cleanupAllThrottled(rt runtime.RuntimeServiceClient, samples []createSample, concurrency int) []cleanupResult {
	var ids []string
	for _, s := range samples {
		if s.id != "" {
			ids = append(ids, s.id)
		}
	}
	results := make([]cleanupResult, 0, len(ids))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			var err error
			for attempt := 0; attempt < cleanupRetry; attempt++ {
				err = cleanup(rt, id)
				if err == nil {
					break
				}
				if !isFlowLimited(err) {
					break
				}
				time.Sleep(cleanupRetryBackoff * time.Duration(attempt+1))
			}
			mu.Lock()
			results = append(results, cleanupResult{id: id, err: err})
			mu.Unlock()
		}(id)
	}
	wg.Wait()
	return results
}

func isFlowLimited(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "130513") || strings.Contains(s, "exceed limited")
}

// staleTapCount reports leftover 192.168.x.x tap interfaces after cleanup.
func staleTapCount() int {
	out, err := exec.Command("ip", "-br", "link").Output()
	if err != nil {
		return -1
	}
	n := 0
	for _, ln := range strings.Split(string(out), "\n") {
		fields := strings.Fields(ln)
		if len(fields) > 0 && strings.HasPrefix(fields[0], "192.168.") {
			n++
		}
	}
	return n
}

func pct(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[len(sorted)-1]
	}
	idx := (p * len(sorted)) / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func summarizeLatencies(label string, n, ok, fail int, wall time.Duration, okTook []time.Duration) {
	sort.Slice(okTook, func(i, j int) bool { return okTook[i] < okTook[j] })
	fmt.Printf("==== %s summary ====\n", label)
	fmt.Printf("n=%d ok=%d fail=%d wall=%s\n", n, ok, fail, wall.Round(time.Millisecond))
	if len(okTook) == 0 {
		return
	}
	var sum time.Duration
	for _, d := range okTook {
		sum += d
	}
	avg := sum / time.Duration(len(okTook))
	p50 := pct(okTook, 50)
	p95 := pct(okTook, 95)
	fmt.Printf("%s latency: avg=%s min=%s p50=%s p95=%s max=%s\n",
		label,
		avg.Round(time.Millisecond),
		okTook[0].Round(time.Millisecond),
		p50.Round(time.Millisecond),
		p95.Round(time.Millisecond),
		okTook[len(okTook)-1].Round(time.Millisecond),
	)
	fmt.Printf("per-sandbox wall amortized: %s\n", (wall / time.Duration(n)).Round(time.Millisecond))
	tps := 0.0
	if wall > 0 {
		tps = float64(ok) / wall.Seconds()
	}
	// 机读行，便于对照对外压测表（client 侧 = CRI/Admin RPC 往返）
	fmt.Printf("[CRIMetric] op=%s n=%d ok=%d fail=%d wall_ms=%d avg_ms=%d p50_ms=%d p95_ms=%d max_ms=%d throughput_per_s=%.1f\n",
		label, n, ok, fail,
		wall.Milliseconds(), avg.Milliseconds(), p50.Milliseconds(), p95.Milliseconds(),
		okTook[len(okTook)-1].Milliseconds(), tps)
}

type hotPod struct {
	idx  int
	uid  string
	id   string
	took time.Duration
	err  error
}

// runConcurrHot：并发冷创建 N → 并发 Pause → 并发 Resume（热启动），只把 Resume 当主指标。
func runConcurrHot(criSock, adminSock, cubeAPI, template string, concurrency int, stepTimeout time.Duration) error {
	if concurrency < 1 {
		concurrency = 1
	}
	ctx := context.Background()
	anns, err := loadColdAnnotations(ctx, cubeAPI, template)
	if err != nil {
		return err
	}
	fmt.Printf("hot n=%d template=%s image=%s cpu=%s mem=%s\n",
		concurrency, anns["cube.dev/template-id"], anns["cube.dev/image"], anns["cube.dev/vcpu"], anns["cube.dev/ram-mb"])

	criConn, err := grpc.NewClient(criSock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer criConn.Close()
	rt := runtime.NewRuntimeServiceClient(criConn)

	adminConn, err := grpc.NewClient(adminSock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer adminConn.Close()
	adm := adminv2.NewSandboxAdminServiceClient(adminConn)

	pods := make([]hotPod, concurrency)
	defer func() {
		for _, p := range pods {
			if p.id != "" {
				_ = cleanup(rt, p.id)
			}
		}
	}()

	// --- setup: concurrent cold create ---
	var wg sync.WaitGroup
	createWall := time.Now()
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			uid := fmt.Sprintf("cube-h-%d-%d", time.Now().UnixNano(), i)
			t0 := time.Now()
			cctx, cancel := context.WithTimeout(ctx, stepTimeout)
			resp, err := rt.RunPodSandbox(cctx, &runtime.RunPodSandboxRequest{
				RuntimeHandler: "cube",
				Config: &runtime.PodSandboxConfig{
					Metadata:    &runtime.PodSandboxMetadata{Name: "cube-h", Uid: uid, Namespace: "default", Attempt: 1},
					Annotations: anns,
					Labels:      map[string]string{"app": "cube-hot"},
				},
			})
			cancel()
			p := hotPod{idx: i, uid: uid, took: time.Since(t0), err: err}
			if err == nil {
				p.id = resp.GetPodSandboxId()
			}
			pods[i] = p
			if err != nil {
				fmt.Printf("create[%d] FAIL took=%s err=%v\n", i, p.took.Round(time.Millisecond), err)
			}
		}(i)
	}
	wg.Wait()
	createTookWall := time.Since(createWall)
	createOK, createFail := 0, 0
	var createLats []time.Duration
	for _, p := range pods {
		if p.err != nil || p.id == "" {
			createFail++
		} else {
			createOK++
			createLats = append(createLats, p.took)
		}
	}
	summarizeLatencies("setup-create", concurrency, createOK, createFail, createTookWall, createLats)
	if createFail > 0 {
		return fmt.Errorf("hot setup create: %d/%d failed", createFail, concurrency)
	}

	// --- setup: concurrent pause ---
	pauseWall := time.Now()
	pauseLats := make([]time.Duration, concurrency)
	pauseErrs := make([]error, concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			t0 := time.Now()
			pctx, cancel := context.WithTimeout(ctx, stepTimeout)
			_, err := adm.PauseSandbox(pctx, &adminv2.PauseSandboxRequest{
				Backend: "cube", OperationId: fmt.Sprintf("hot-pause-%d-%d", time.Now().UnixNano(), i),
				Target:         &adminv2.PauseSandboxRequest_CriSandboxId{CriSandboxId: pods[i].id},
				TimeoutSeconds: uint64(stepTimeout.Seconds()),
			})
			cancel()
			pauseLats[i] = time.Since(t0)
			pauseErrs[i] = err
			if err != nil {
				fmt.Printf("pause[%d] FAIL took=%s err=%v\n", i, pauseLats[i].Round(time.Millisecond), err)
			}
		}(i)
	}
	wg.Wait()
	pauseTookWall := time.Since(pauseWall)
	pauseOK, pauseFail := 0, 0
	var pauseOKLats []time.Duration
	for i, err := range pauseErrs {
		if err != nil {
			pauseFail++
		} else {
			pauseOK++
			pauseOKLats = append(pauseOKLats, pauseLats[i])
		}
	}
	summarizeLatencies("setup-pause", concurrency, pauseOK, pauseFail, pauseTookWall, pauseOKLats)
	if pauseFail > 0 {
		return fmt.Errorf("hot setup pause: %d/%d failed", pauseFail, concurrency)
	}

	// --- measured: concurrent resume (热启动) ---
	fmt.Println("== concurrent resume (hot start) ==")
	resumeWall := time.Now()
	resumeLats := make([]time.Duration, concurrency)
	resumeErrs := make([]error, concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			t0 := time.Now()
			rctx, cancel := context.WithTimeout(ctx, stepTimeout)
			_, err := rt.RunPodSandbox(rctx, &runtime.RunPodSandboxRequest{
				RuntimeHandler: "cube",
				Config: &runtime.PodSandboxConfig{
					Metadata:    &runtime.PodSandboxMetadata{Name: "cube-h", Uid: pods[i].uid, Namespace: "default", Attempt: 1},
					Annotations: anns,
				},
			})
			cancel()
			resumeLats[i] = time.Since(t0)
			if err != nil {
				resumeErrs[i] = err
				fmt.Printf("resume[%d] FAIL took=%s err=%v\n", i, resumeLats[i].Round(time.Millisecond), err)
				return
			}
			st, stErr := rt.PodSandboxStatus(context.Background(), &runtime.PodSandboxStatusRequest{PodSandboxId: pods[i].id})
			if stErr != nil {
				resumeErrs[i] = stErr
				fmt.Printf("resume[%d] status FAIL err=%v\n", i, stErr)
				return
			}
			if st.GetStatus().GetState() != runtime.PodSandboxState_SANDBOX_READY {
				resumeErrs[i] = fmt.Errorf("state=%v", st.GetStatus().GetState())
				fmt.Printf("resume[%d] not READY state=%v\n", i, st.GetStatus().GetState())
				return
			}
			fmt.Printf("resume[%d] OK took=%s ip=%s\n", i, resumeLats[i].Round(time.Millisecond), st.GetStatus().GetNetwork().GetIp())
		}(i)
	}
	wg.Wait()
	resumeTookWall := time.Since(resumeWall)
	resumeOK, resumeFail := 0, 0
	var resumeOKLats []time.Duration
	for i, err := range resumeErrs {
		if err != nil {
			resumeFail++
		} else {
			resumeOK++
			resumeOKLats = append(resumeOKLats, resumeLats[i])
		}
	}
	fmt.Println()
	summarizeLatencies("hot-resume", concurrency, resumeOK, resumeFail, resumeTookWall, resumeOKLats)
	if resumeOK > 0 && pauseOK > 0 {
		var pSum, rSum time.Duration
		for _, d := range pauseOKLats {
			pSum += d
		}
		for _, d := range resumeOKLats {
			rSum += d
		}
		pAvg := pSum / time.Duration(len(pauseOKLats))
		rAvg := rSum / time.Duration(len(resumeOKLats))
		fmt.Printf("[CRIMetric] op=pause+resume n=%d avg_ms=%d (pause_avg_ms=%d resume_avg_ms=%d)\n",
			concurrency, (pAvg + rAvg).Milliseconds(), pAvg.Milliseconds(), rAvg.Milliseconds())
	}
	if resumeFail > 0 {
		return fmt.Errorf("hot resume: %d/%d failed", resumeFail, concurrency)
	}
	return nil
}
