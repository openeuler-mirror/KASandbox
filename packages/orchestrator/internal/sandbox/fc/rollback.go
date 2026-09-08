package fc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

)

// RollbackResult is the forked Firecracker's response to a successful
// in-place rollback.
type RollbackResult struct {
	RestoredPages int64 `json:"restored_pages"`
	RestoredBytes int64 `json:"restored_bytes"`
	TimingsUs     struct {
		Validate uint64 `json:"validate"`
		Quiesce  uint64 `json:"quiesce"`
		Memory   uint64 `json:"memory"`
		Vcpus    uint64 `json:"vcpus"`
		Gic      uint64 `json:"gic"`
		Devices  uint64 `json:"devices"`
		Total    uint64 `json:"total"`
	} `json:"timings_us"`
}

// RollbackUnsupportedError marks a Firecracker that does not know the
// rollback endpoint — an unmodified binary. The orchestrator and Firecracker
// ship as a pair; mismatched binaries fail the restore rather than degrade.
type RollbackUnsupportedError struct{}

func (RollbackUnsupportedError) Error() string {
	return "this firecracker does not support in-place rollback"
}

// RollbackFaultedError marks a rollback that failed past its commit point:
// the VM is torn between two moments in time, refuses to resume, and must be
// replaced. The caller's only move is the kill-and-rebuild fallback.
type RollbackFaultedError struct {
	Message string
}

func (e RollbackFaultedError) Error() string {
	return fmt.Sprintf("rollback failed past the commit point, VM is faulted: %s", e.Message)
}

// rollbackSnapshot calls the forked Firecracker's PUT /snapshot/rollback.
// The endpoint is not in the generated OpenAPI client (it exists only in the
// fork), so this is a plain HTTP call over the same API socket.
func (c *apiClient) rollbackSnapshot(
	ctx context.Context,
	socketPath string,
	snapfilePath string,
	memFilePath string,
	revertBitmapPath string,
) (*RollbackResult, error) {
	body := map[string]any{
		"snapshot_path": snapfilePath,
		"mem_file_path": memFilePath,
		// The VM is resumed by the orchestrator after the disk view has been
		// switched, not by Firecracker.
		"resume_vm": false,
	}
	if revertBitmapPath != "" {
		body["revert_bitmap_path"] = revertBitmapPath
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("error encoding rollback request: %w", err)
	}

	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer

				return d.DialContext(ctx, "unix", socketPath)
			},
			// Firecracker's API server caps concurrent connections. This
			// client is built per call, so a pooled connection would stay
			// open until the transport is collected: a sandbox rolled back
			// often enough exhausts the cap and every later rollback is
			// refused with 503.
			DisableKeepAlives: true,
		},
		// Memory write-back is proportional to the revert set; a large one
		// takes a while but never forever.
		Timeout: 2 * time.Minute,
	}
	defer httpClient.CloseIdleConnections()

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPut,
		"http://localhost/snapshot/rollback",
		bytes.NewReader(payload),
	)
	if err != nil {
		return nil, fmt.Errorf("error building rollback request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error calling rollback: %w", err)
	}
	defer res.Body.Close()

	resBody, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("error reading rollback response: %w", err)
	}

	switch {
	case res.StatusCode == http.StatusOK || res.StatusCode == http.StatusNoContent:
		result := &RollbackResult{}
		if len(resBody) > 0 {
			if err := json.Unmarshal(resBody, result); err != nil {
				return nil, fmt.Errorf("error decoding rollback response: %w", err)
			}
		}

		return result, nil
	case res.StatusCode == http.StatusNotFound:
		// An unmodified Firecracker routes unknown /snapshot/* to 404.
		return nil, RollbackUnsupportedError{}
	default:
		message := string(resBody)
		// The fork brands the VM Faulted on post-commit failures and says so
		// in the error; everything else left the VM resumable.
		if bytes.Contains(resBody, []byte("Faulted")) || bytes.Contains(resBody, []byte("faulted")) {
			return nil, RollbackFaultedError{Message: message}
		}

		return nil, fmt.Errorf("rollback failed with status %d: %s", res.StatusCode, message)
	}
}

// saveDirtyBitmap calls the forked Firecracker's PUT
// /snapshot/save-dirty-bitmap: the live dirty bitmap is written to path in
// the FCDB sidecar format. Requires the VM to be paused.
func (c *apiClient) saveDirtyBitmap(ctx context.Context, socketPath string, path string) error {
	payload, err := json.Marshal(map[string]any{"path": path})
	if err != nil {
		return fmt.Errorf("error encoding save-dirty-bitmap request: %w", err)
	}

	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer

				return d.DialContext(ctx, "unix", socketPath)
			},
			// Same reason as rollbackSnapshot: this client lives for one
			// call, and pooled connections would pile up against
			// Firecracker's connection cap.
			DisableKeepAlives: true,
		},
		Timeout: 30 * time.Second,
	}
	defer httpClient.CloseIdleConnections()

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPut,
		"http://localhost/snapshot/save-dirty-bitmap",
		bytes.NewReader(payload),
	)
	if err != nil {
		return fmt.Errorf("error building save-dirty-bitmap request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("error calling save-dirty-bitmap: %w", err)
	}
	defer res.Body.Close()

	resBody, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode == http.StatusOK || res.StatusCode == http.StatusNoContent {
		return nil
	}

	return fmt.Errorf("save-dirty-bitmap failed with status %d: %s", res.StatusCode, string(resBody))
}
