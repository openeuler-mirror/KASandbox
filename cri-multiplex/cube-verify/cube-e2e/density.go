package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func runDensity(criSock, cubeAPI, template string, count int, stepTimeout time.Duration) error {
	if count < 1 {
		count = 1
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

	baseAvail, err := memAvailableMB()
	if err != nil {
		return err
	}
	fmt.Printf("density baseline available_mb=%d count=%d\n", baseAvail, count)

	ids := make([]string, 0, count)
	defer func() {
		for _, id := range ids {
			_ = cleanup(rt, id)
		}
	}()

	for i := 0; i < count; i++ {
		uid := fmt.Sprintf("cube-den-%d-%d", time.Now().UnixNano(), i)
		cctx, cancel := context.WithTimeout(ctx, stepTimeout)
		resp, err := rt.RunPodSandbox(cctx, &runtime.RunPodSandboxRequest{
			RuntimeHandler: "cube",
			Config: &runtime.PodSandboxConfig{
				Metadata:    &runtime.PodSandboxMetadata{Name: "cube-den", Uid: uid, Namespace: "default", Attempt: 1},
				Annotations: anns,
			},
		})
		cancel()
		if err != nil {
			return fmt.Errorf("create %d: %w", i, err)
		}
		ids = append(ids, resp.GetPodSandboxId())
		if (i+1)%5 == 0 || i+1 == count {
			avail, _ := memAvailableMB()
			used := baseAvail - avail
			per := float64(used) / float64(i+1)
			fmt.Printf("alive=%d available_mb=%d delta_mb=%d per_vm_mb=%.1f\n", i+1, avail, used, per)
		}
	}
	avail, _ := memAvailableMB()
	used := baseAvail - avail
	per := float64(used) / float64(count)
	fmt.Println()
	fmt.Printf("density summary: n=%d delta_mb=%d per_vm_mb=%.1f\n", count, used, per)
	fmt.Println("published empty-VM overhead (PVM blog): ~27–34 MB/VM")
	return nil
}

func memAvailableMB() (int64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "MemAvailable:") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				break
			}
			kb, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0, err
			}
			return kb / 1024, nil
		}
	}
	return 0, fmt.Errorf("MemAvailable not found")
}
