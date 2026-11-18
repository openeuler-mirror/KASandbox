package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	adminv2 "github.com/cri-multiplex/pkg/admin/v2"
)

type sampleSet struct {
	Name string
	Xs   []time.Duration
}

func (s *sampleSet) add(d time.Duration) { s.Xs = append(s.Xs, d) }

func (s sampleSet) report() string {
	if len(s.Xs) == 0 {
		return s.Name + ": n=0"
	}
	xs := append([]time.Duration(nil), s.Xs...)
	sort.Slice(xs, func(i, j int) bool { return xs[i] < xs[j] })
	var sum time.Duration
	for _, x := range xs {
		sum += x
	}
	avg := sum / time.Duration(len(xs))
	p := func(p float64) time.Duration {
		if len(xs) == 1 {
			return xs[0]
		}
		idx := int(float64(len(xs)-1) * p)
		return xs[idx]
	}
	return fmt.Sprintf("%s: n=%d avg=%s min=%s p50=%s p95=%s max=%s",
		s.Name, len(xs),
		avg.Round(time.Millisecond),
		xs[0].Round(time.Millisecond),
		p(0.50).Round(time.Millisecond),
		p(0.95).Round(time.Millisecond),
		xs[len(xs)-1].Round(time.Millisecond),
	)
}

func runBench(criSock, adminSock, cubeAPI, template string, rounds int, stepTimeout time.Duration) error {
	if rounds < 1 {
		rounds = 1
	}
	ctx := context.Background()
	anns, err := loadColdAnnotations(ctx, cubeAPI, template)
	if err != nil {
		return err
	}
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

	createS := sampleSet{Name: "create"}
	execS := sampleSet{Name: "exec_health"}
	cpS := sampleSet{Name: "checkpoint"}
	pauseS := sampleSet{Name: "pause"}
	resumeS := sampleSet{Name: "resume"}

	fmt.Printf("bench rounds=%d template=%s\n", rounds, anns["cube.dev/template-id"])
	for i := 0; i < rounds; i++ {
		uid := fmt.Sprintf("cube-bench-%d-%d", time.Now().UnixNano(), i)
		t0 := time.Now()
		cctx, cancel := context.WithTimeout(ctx, stepTimeout)
		resp, err := rt.RunPodSandbox(cctx, &runtime.RunPodSandboxRequest{
			RuntimeHandler: "cube",
			Config: &runtime.PodSandboxConfig{
				Metadata:    &runtime.PodSandboxMetadata{Name: "cube-bench", Uid: uid, Namespace: "default", Attempt: 1},
				Annotations: anns,
			},
		})
		cancel()
		createTook := time.Since(t0)
		if err != nil {
			return fmt.Errorf("round %d create: %w", i, err)
		}
		createS.add(createTook)
		criID := resp.GetPodSandboxId()
		st, err := rt.PodSandboxStatus(ctx, &runtime.PodSandboxStatusRequest{PodSandboxId: criID})
		if err != nil {
			_ = cleanup(rt, criID)
			return err
		}
		ip := st.GetStatus().GetNetwork().GetIp()

		// 先等到 envd 就绪（不计时），再测一次 /health 往返作为沙箱内执行时延。
		if err := waitEnvd(ip, 49983, "/health", 15*time.Second); err != nil {
			_ = cleanup(rt, criID)
			return fmt.Errorf("round %d wait envd: %w", i, err)
		}
		t0 = time.Now()
		code, err := getEnvd(ip, 49983, "/health", 2*time.Second)
		execTook := time.Since(t0)
		if err != nil || (code != 200 && code != 204) {
			_ = cleanup(rt, criID)
			return fmt.Errorf("round %d exec_health ip=%s code=%d err=%v", i, ip, code, err)
		}
		execS.add(execTook)

		t0 = time.Now()
		cpCtx, cpCancel := context.WithTimeout(ctx, stepTimeout)
		cp, err := adm.CheckpointSandbox(cpCtx, &adminv2.CheckpointSandboxRequest{
			Backend: "cube", OperationId: fmt.Sprintf("bench-cp-%d", i),
			Target:         &adminv2.CheckpointSandboxRequest_CriSandboxId{CriSandboxId: criID},
			TimeoutSeconds: uint64(stepTimeout.Seconds()),
		})
		cpCancel()
		if err != nil || !cp.GetSnapshotPersisted() {
			_ = cleanup(rt, criID)
			return fmt.Errorf("round %d checkpoint: %v %v", i, err, cp)
		}
		cpS.add(time.Since(t0))

		t0 = time.Now()
		pCtx, pCancel := context.WithTimeout(ctx, stepTimeout)
		pause, err := adm.PauseSandbox(pCtx, &adminv2.PauseSandboxRequest{
			Backend: "cube", OperationId: fmt.Sprintf("bench-pause-%d", i),
			Target:         &adminv2.PauseSandboxRequest_CriSandboxId{CriSandboxId: criID},
			TimeoutSeconds: uint64(stepTimeout.Seconds()),
		})
		pCancel()
		if err != nil || !pause.GetSnapshotPersisted() {
			_ = cleanup(rt, criID)
			return fmt.Errorf("round %d pause: %v", i, err)
		}
		pauseS.add(time.Since(t0))

		t0 = time.Now()
		rCtx, rCancel := context.WithTimeout(ctx, stepTimeout)
		_, err = rt.RunPodSandbox(rCtx, &runtime.RunPodSandboxRequest{
			RuntimeHandler: "cube",
			Config: &runtime.PodSandboxConfig{
				Metadata:    &runtime.PodSandboxMetadata{Name: "cube-bench", Uid: uid, Namespace: "default", Attempt: 1},
				Annotations: anns,
			},
		})
		rCancel()
		if err != nil {
			_ = cleanup(rt, criID)
			return fmt.Errorf("round %d resume: %w", i, err)
		}
		resumeS.add(time.Since(t0))

		if err := cleanup(rt, criID); err != nil {
			return fmt.Errorf("round %d remove: %w", i, err)
		}
		fmt.Printf("round %d/%d create=%s exec=%s pause=%s resume=%s\n",
			i+1, rounds,
			createTook.Round(time.Millisecond),
			execTook.Round(time.Millisecond),
			pauseS.Xs[len(pauseS.Xs)-1].Round(time.Millisecond),
			resumeS.Xs[len(resumeS.Xs)-1].Round(time.Millisecond),
		)
	}

	fmt.Println()
	fmt.Println("==== CRI cube bench vs published CubeAPI (PVM blog) ====")
	fmt.Println(createS.report())
	fmt.Println("  published create c=1: avg=66.7ms min=55.9 p50=64.5 p95=78.2")
	fmt.Println(execS.report())
	fmt.Println("  published: no direct envd /health number; blog uses create-to-running / SDK run_code")
	fmt.Println(cpS.report())
	fmt.Println("  published snapshot c=1 (~8MB dirty): avg=41.4ms")
	fmt.Println(pauseS.report())
	fmt.Println("  published pause c=1 (full-memory-copy 2GiB): avg=370.8ms")
	fmt.Println(resumeS.report())
	fmt.Println("  published resume c=1: avg=18.9ms")
	return nil
}

func getEnvd(ip string, port int, path string, timeout time.Duration) (int, error) {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(fmt.Sprintf("http://%s:%d%s", ip, port, path))
	if err != nil {
		return 0, err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, nil
}

func waitEnvd(ip string, port int, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		code, err := getEnvd(ip, port, path, time.Second)
		if err == nil && (code == 200 || code == 204) {
			return nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("status %d", code)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return lastErr
}
