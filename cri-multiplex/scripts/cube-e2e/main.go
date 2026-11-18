// cube-e2e 对真 Cubelet 跑 CRI Create / Admin Checkpoint / Pause / Resume / Remove，
// 并打印各阶段耗时（尤其是 cold create）。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	adminv2 "github.com/cri-multiplex/pkg/admin/v2"
)

type timing struct {
	Name string
	Took time.Duration
	OK   bool
	Err  string
	Note string
}

func main() {
	criSock := flag.String("cri-socket", "unix:///tmp/cri-e2e/cri.sock", "cri-multiplex CRI unix socket")
	adminSock := flag.String("admin-socket", "unix:///tmp/cri-e2e/admin.sock", "cri-multiplex admin unix socket")
	cubeAPI := flag.String("cube-api", envOr("CUBE_API_URL", "http://127.0.0.1:3000"), "CubeAPI base URL for template lookup")
	template := flag.String("template", envOr("CUBE_E2E_TEMPLATE", "sandbox-code"), "READY template id or alias")
	timeout := flag.Duration("timeout", 3*time.Minute, "per-step timeout")
	mode := flag.String("mode", "e2e", "e2e | hold | remove | bench | density | concurr | hot")
	criIDFlag := flag.String("cri-id", "", "CRI sandbox id for -mode remove")
	rounds := flag.Int("n", 1, "bench rounds / density count / concurr|hot total requests")
	conc := flag.Int("c", 0, "concurr concurrency (0 = same as -n, one-shot wave; cube-bench style: -c workers × -n total)")
	flag.Parse()

	var err error
	switch *mode {
	case "bench":
		err = runBench(*criSock, *adminSock, *cubeAPI, *template, *rounds, *timeout)
	case "density":
		err = runDensity(*criSock, *cubeAPI, *template, *rounds, *timeout)
	case "concurr":
		err = runConcurr(*criSock, *cubeAPI, *template, *rounds, *conc, *timeout)
	case "hot":
		err = runConcurrHot(*criSock, *adminSock, *cubeAPI, *template, *rounds, *timeout)
	case "hold":
		criID, sbxID, holdErr := hold(*criSock, *adminSock, *cubeAPI, *template, *timeout)
		if holdErr == nil {
			fmt.Printf("CRI_ID=%s\nSANDBOX_ID=%s\n", criID, sbxID)
		}
		err = holdErr
	case "remove":
		err = removeOnly(*criSock, *criIDFlag)
	default:
		timings, runErr := run(*criSock, *adminSock, *cubeAPI, *template, *timeout)
		printTimings(timings)
		err = runErr
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("PASS")
}

func run(criSock, adminSock, cubeAPI, template string, stepTimeout time.Duration) ([]timing, error) {
	var timings []timing
	ctx := context.Background()

	anns, err := loadColdAnnotations(ctx, cubeAPI, template)
	if err != nil {
		return timings, fmt.Errorf("load template %s: %w", template, err)
	}
	fmt.Printf("template=%s image=%s cpu=%s mem=%s\n",
		anns["cube.dev/template-id"], anns["cube.dev/image"], anns["cube.dev/vcpu"], anns["cube.dev/ram-mb"])

	criConn, err := grpc.NewClient(criSock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return timings, err
	}
	defer criConn.Close()
	rt := runtime.NewRuntimeServiceClient(criConn)

	adminConn, err := grpc.NewClient(adminSock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return timings, err
	}
	defer adminConn.Close()
	adm := adminv2.NewSandboxAdminServiceClient(adminConn)

	uid := fmt.Sprintf("cube-e2e-%d", time.Now().UnixNano())

	fmt.Println("== Create (RunPodSandbox cold) ==")
	createStart := time.Now()
	createCtx, cancel := context.WithTimeout(ctx, stepTimeout)
	runResp, err := rt.RunPodSandbox(createCtx, &runtime.RunPodSandboxRequest{
		RuntimeHandler: "cube",
		Config: &runtime.PodSandboxConfig{
			Metadata: &runtime.PodSandboxMetadata{
				Name: "cube-e2e", Uid: uid, Namespace: "default", Attempt: 1,
			},
			Annotations: anns,
			Labels:      map[string]string{"app": "cube-e2e"},
		},
	})
	cancel()
	createTook := time.Since(createStart)
	if err != nil {
		timings = append(timings, timing{Name: "create", Took: createTook, OK: false, Err: err.Error()})
		return timings, fmt.Errorf("create: %w", err)
	}
	criID := runResp.GetPodSandboxId()
	st, err := rt.PodSandboxStatus(ctx, &runtime.PodSandboxStatusRequest{PodSandboxId: criID})
	if err != nil {
		timings = append(timings, timing{Name: "create", Took: createTook, OK: false, Err: err.Error()})
		return timings, err
	}
	ip := st.GetStatus().GetNetwork().GetIp()
	if st.GetStatus().GetState() != runtime.PodSandboxState_SANDBOX_READY || ip == "" {
		timings = append(timings, timing{Name: "create", Took: createTook, OK: false, Err: "not READY with ip"})
		return timings, fmt.Errorf("create: state=%v ip=%q", st.GetStatus().GetState(), ip)
	}
	timings = append(timings, timing{Name: "create", Took: createTook, OK: true, Note: "ip=" + ip})
	fmt.Printf("cri=%s ip=%s took=%s\n", criID, ip, createTook)

	fmt.Println("== GetSandboxRuntime ==")
	t0 := time.Now()
	rtResp, err := adm.GetSandboxRuntime(ctx, &adminv2.GetSandboxRuntimeRequest{
		Backend: "cube",
		Target:  &adminv2.GetSandboxRuntimeRequest_CriSandboxId{CriSandboxId: criID},
	})
	took := time.Since(t0)
	if err != nil {
		timings = append(timings, timing{Name: "get_runtime", Took: took, OK: false, Err: err.Error()})
		return timings, err
	}
	sbxID := rtResp.GetRuntime().GetSandboxId()
	timings = append(timings, timing{Name: "get_runtime", Took: took, OK: true, Note: "sandbox_id=" + sbxID})
	fmt.Printf("sandbox_id=%s state=%s took=%s\n", sbxID, rtResp.GetRuntime().GetRuntimeState(), took)

	fmt.Println("== Checkpoint ==")
	t0 = time.Now()
	cpCtx, cpCancel := context.WithTimeout(ctx, stepTimeout)
	cp, err := adm.CheckpointSandbox(cpCtx, &adminv2.CheckpointSandboxRequest{
		Backend: "cube", OperationId: "e2e-cp-1",
		Target:         &adminv2.CheckpointSandboxRequest_CriSandboxId{CriSandboxId: criID},
		TimeoutSeconds: uint64(stepTimeout.Seconds()),
	})
	cpCancel()
	took = time.Since(t0)
	if err != nil {
		timings = append(timings, timing{Name: "checkpoint", Took: took, OK: false, Err: err.Error()})
		_ = cleanup(rt, criID)
		return timings, err
	}
	if !cp.GetSnapshotPersisted() || !cp.GetVmResumed() {
		timings = append(timings, timing{Name: "checkpoint", Took: took, OK: false, Err: "bad flags"})
		_ = cleanup(rt, criID)
		return timings, fmt.Errorf("checkpoint flags persisted=%v resumed=%v", cp.GetSnapshotPersisted(), cp.GetVmResumed())
	}
	timings = append(timings, timing{Name: "checkpoint", Took: took, OK: true})
	fmt.Printf("checkpoint ok took=%s\n", took)

	fmt.Println("== Pause ==")
	t0 = time.Now()
	pauseCtx, pauseCancel := context.WithTimeout(ctx, stepTimeout)
	pause, err := adm.PauseSandbox(pauseCtx, &adminv2.PauseSandboxRequest{
		Backend: "cube", OperationId: "e2e-pause-1",
		Target:         &adminv2.PauseSandboxRequest_CriSandboxId{CriSandboxId: criID},
		TimeoutSeconds: uint64(stepTimeout.Seconds()),
	})
	pauseCancel()
	took = time.Since(t0)
	if err != nil {
		timings = append(timings, timing{Name: "pause", Took: took, OK: false, Err: err.Error()})
		_ = cleanup(rt, criID)
		return timings, err
	}
	if !pause.GetSnapshotPersisted() {
		timings = append(timings, timing{Name: "pause", Took: took, OK: false, Err: "not persisted"})
		_ = cleanup(rt, criID)
		return timings, fmt.Errorf("pause not persisted")
	}
	st, err = rt.PodSandboxStatus(ctx, &runtime.PodSandboxStatusRequest{PodSandboxId: criID})
	if err != nil || st.GetStatus().GetState() != runtime.PodSandboxState_SANDBOX_NOTREADY {
		timings = append(timings, timing{Name: "pause", Took: took, OK: false, Err: "not NOTREADY"})
		_ = cleanup(rt, criID)
		return timings, fmt.Errorf("pause status: %v", err)
	}
	timings = append(timings, timing{Name: "pause", Took: took, OK: true})
	fmt.Printf("pause ok took=%s\n", took)

	fmt.Println("== Resume (RunPodSandbox same uid) ==")
	t0 = time.Now()
	resumeCtx, resumeCancel := context.WithTimeout(ctx, stepTimeout)
	_, err = rt.RunPodSandbox(resumeCtx, &runtime.RunPodSandboxRequest{
		RuntimeHandler: "cube",
		Config: &runtime.PodSandboxConfig{
			Metadata:    &runtime.PodSandboxMetadata{Name: "cube-e2e", Uid: uid, Namespace: "default", Attempt: 1},
			Annotations: anns,
		},
	})
	resumeCancel()
	took = time.Since(t0)
	if err != nil {
		timings = append(timings, timing{Name: "resume", Took: took, OK: false, Err: err.Error()})
		_ = cleanup(rt, criID)
		return timings, err
	}
	st, err = rt.PodSandboxStatus(ctx, &runtime.PodSandboxStatusRequest{PodSandboxId: criID})
	if err != nil || st.GetStatus().GetState() != runtime.PodSandboxState_SANDBOX_READY {
		timings = append(timings, timing{Name: "resume", Took: took, OK: false, Err: "not READY"})
		_ = cleanup(rt, criID)
		return timings, fmt.Errorf("resume status: %v", err)
	}
	rtResp, err = adm.GetSandboxRuntime(ctx, &adminv2.GetSandboxRuntimeRequest{
		Backend: "cube",
		Target:  &adminv2.GetSandboxRuntimeRequest_SandboxId{SandboxId: sbxID},
	})
	if err != nil || rtResp.GetRuntime().GetSandboxId() != sbxID {
		timings = append(timings, timing{Name: "resume", Took: took, OK: false, Err: "sandbox id changed"})
		_ = cleanup(rt, criID)
		return timings, fmt.Errorf("resume sandbox id mismatch")
	}
	timings = append(timings, timing{
		Name: "resume", Took: took, OK: true,
		Note: fmt.Sprintf("ip=%s", st.GetStatus().GetNetwork().GetIp()),
	})
	fmt.Printf("resume ok sandbox_id=%s ip=%s took=%s\n", sbxID, st.GetStatus().GetNetwork().GetIp(), took)

	fmt.Println("== Remove ==")
	t0 = time.Now()
	err = cleanup(rt, criID)
	took = time.Since(t0)
	if err != nil {
		timings = append(timings, timing{Name: "remove", Took: took, OK: false, Err: err.Error()})
		return timings, err
	}
	timings = append(timings, timing{Name: "remove", Took: took, OK: true})
	fmt.Printf("remove ok took=%s\n", took)
	return timings, nil
}

// hold 只做冷启动并留下沙箱，供经典 pause/delete 护栏测试。失败时清掉自己创建的沙箱。
func hold(criSock, adminSock, cubeAPI, template string, stepTimeout time.Duration) (string, string, error) {
	ctx := context.Background()
	anns, err := loadColdAnnotations(ctx, cubeAPI, template)
	if err != nil {
		return "", "", fmt.Errorf("load template %s: %w", template, err)
	}
	criConn, err := grpc.NewClient(criSock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return "", "", err
	}
	defer criConn.Close()
	rt := runtime.NewRuntimeServiceClient(criConn)
	adminConn, err := grpc.NewClient(adminSock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return "", "", err
	}
	defer adminConn.Close()
	adm := adminv2.NewSandboxAdminServiceClient(adminConn)

	uid := fmt.Sprintf("cube-hold-%d", time.Now().UnixNano())
	createCtx, cancel := context.WithTimeout(ctx, stepTimeout)
	runResp, err := rt.RunPodSandbox(createCtx, &runtime.RunPodSandboxRequest{
		RuntimeHandler: "cube",
		Config: &runtime.PodSandboxConfig{
			Metadata: &runtime.PodSandboxMetadata{
				Name: "cube-hold", Uid: uid, Namespace: "default", Attempt: 1,
			},
			Annotations: anns,
			Labels:      map[string]string{"app": "cube-hold"},
		},
	})
	cancel()
	if err != nil {
		return "", "", fmt.Errorf("create: %w", err)
	}
	criID := runResp.GetPodSandboxId()
	st, err := rt.PodSandboxStatus(ctx, &runtime.PodSandboxStatusRequest{PodSandboxId: criID})
	if err != nil || st.GetStatus().GetState() != runtime.PodSandboxState_SANDBOX_READY || st.GetStatus().GetNetwork().GetIp() == "" {
		_ = cleanup(rt, criID)
		if err != nil {
			return "", "", err
		}
		return "", "", fmt.Errorf("create: state=%v ip=%q", st.GetStatus().GetState(), st.GetStatus().GetNetwork().GetIp())
	}
	rtResp, err := adm.GetSandboxRuntime(ctx, &adminv2.GetSandboxRuntimeRequest{
		Backend: "cube",
		Target:  &adminv2.GetSandboxRuntimeRequest_CriSandboxId{CriSandboxId: criID},
	})
	if err != nil || rtResp.GetRuntime().GetSandboxId() == "" {
		_ = cleanup(rt, criID)
		if err != nil {
			return "", "", err
		}
		return "", "", fmt.Errorf("empty sandbox id")
	}
	return criID, rtResp.GetRuntime().GetSandboxId(), nil
}

func removeOnly(criSock, criID string) error {
	if criID == "" {
		return fmt.Errorf("-cri-id is required")
	}
	conn, err := grpc.NewClient(criSock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	return cleanup(runtime.NewRuntimeServiceClient(conn), criID)
}

func cleanup(rt runtime.RuntimeServiceClient, criID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, _ = rt.StopPodSandbox(ctx, &runtime.StopPodSandboxRequest{PodSandboxId: criID})
	_, err := rt.RemovePodSandbox(ctx, &runtime.RemovePodSandboxRequest{PodSandboxId: criID})
	return err
}

func printTimings(timings []timing) {
	fmt.Println()
	fmt.Println("---- timing summary ----")
	fmt.Printf("%-12s %-8s %-12s %s\n", "step", "ok", "took", "note/error")
	for _, t := range timings {
		ok := "PASS"
		extra := t.Note
		if !t.OK {
			ok = "FAIL"
			extra = t.Err
		}
		fmt.Printf("%-12s %-8s %-12s %s\n", t.Name, ok, t.Took.Round(time.Millisecond), extra)
	}
	fmt.Println("------------------------")
}

func loadColdAnnotations(ctx context.Context, cubeAPI, template string) (map[string]string, error) {
	template = strings.TrimSpace(template)
	if template == "" {
		return nil, fmt.Errorf("template is empty")
	}
	url := strings.TrimRight(cubeAPI, "/") + "/templates/" + template
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s %s", url, resp.Status, strings.TrimSpace(string(body)))
	}
	var detail struct {
		TemplateID    string `json:"templateID"`
		Status        string `json:"status"`
		CreateRequest *struct {
			Annotations map[string]string `json:"annotations"`
			Volumes     []struct {
				Name string `json:"name"`
			} `json:"volumes"`
			Containers []struct {
				Name      string `json:"name"`
				Args      []string
				Command   []string
				Resources struct {
					CPU string `json:"cpu"`
					Mem string `json:"mem"`
				} `json:"resources"`
				Image *struct {
					Image        string            `json:"image"`
					StorageMedia string            `json:"storage_media"`
					Annotations  map[string]string `json:"annotations"`
				} `json:"image"`
				VolumeMounts []struct {
					Name          string `json:"name"`
					ContainerPath string `json:"container_path"`
				} `json:"volume_mounts"`
			} `json:"containers"`
		} `json:"createRequest"`
	}
	if err := json.Unmarshal(body, &detail); err != nil {
		return nil, err
	}
	if !strings.EqualFold(detail.Status, "READY") {
		return nil, fmt.Errorf("template status %q, want READY", detail.Status)
	}
	if detail.CreateRequest == nil || len(detail.CreateRequest.Containers) == 0 {
		return nil, fmt.Errorf("template createRequest/containers missing")
	}
	tplID := detail.TemplateID
	if tplID == "" {
		tplID = template
	}
	c0 := detail.CreateRequest.Containers[0]
	if c0.Image == nil || c0.Image.Image == "" {
		return nil, fmt.Errorf("template container image missing")
	}
	vol := "cube_rootfs_rw"
	disk := "1G"
	if len(detail.CreateRequest.Volumes) > 0 && detail.CreateRequest.Volumes[0].Name != "" {
		vol = detail.CreateRequest.Volumes[0].Name
	}
	if v := detail.CreateRequest.Annotations["cube.master.rootfs.writable_layer_size"]; v != "" {
		disk = v
	}
	anns := map[string]string{
		"cube.dev/template-id":         tplID,
		"cube.dev/vcpu":                c0.Resources.CPU,
		"cube.dev/ram-mb":              c0.Resources.Mem,
		"cube.dev/disk-mb":             disk,
		"cube.dev/container-name":      c0.Name,
		"cube.dev/volume-name":         vol,
		"cube.dev/image":               c0.Image.Image,
		"cube.dev/image-storage-media": c0.Image.StorageMedia,
		"cube.master.appsnapshot.version": "v2",
	}
	if len(c0.Args) > 0 {
		b, _ := json.Marshal(c0.Args)
		anns["cube.dev/args"] = string(b)
	}
	if len(c0.Command) > 0 {
		b, _ := json.Marshal(c0.Command)
		anns["cube.dev/command"] = string(b)
	}
	for k, v := range c0.Image.Annotations {
		if strings.HasPrefix(k, "cube.master.rootfs.") || k == "cube.master.template.spec_fingerprint" {
			anns[k] = v
		}
	}
	for k, v := range detail.CreateRequest.Annotations {
		if strings.HasPrefix(k, "cube.master.rootfs.") || k == "cube.master.template.spec_fingerprint" {
			if _, ok := anns[k]; !ok {
				anns[k] = v
			}
		}
	}
	return anns, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
