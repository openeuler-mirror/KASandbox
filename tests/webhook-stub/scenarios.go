package main

import (
	"fmt"
	"net/http"
	"time"
)

// state carries data across scenarios: later scenarios reuse resources
// (template IDs, build IDs) created by earlier ones.
type state struct {
	client  *Client
	teamID  string
	baseEnv string

	// scenario 3/4: pause flow on pauseSandboxID
	pauseTemplateID string
	pauseBuildID1   string
	pauseBuildID2   string

	// scenario 6: checkpoint flow on checkpointSandboxID
	cpSnapshotID string
	cpTemplateID string
	cpBuildID    string
}

const (
	pauseSandboxID     = "stub-sandbox-0001"
	checkpointSandboxID = "stub-sandbox-0002"
)

// snapshotBody builds a valid RuntimeSnapshotMetadata request body.
// Field names verified against spec/openapi.yml RuntimeSnapshotMetadata schema.
func (s *state) snapshotBody(operationID, sourceNode string, vcpu int64) map[string]any {
	return map[string]any{
		"operation_id":         operationID,
		"team_id":              s.teamID,
		"base_template_id":     s.baseEnv,
		"source_pod_uid":       "pod-uid-" + operationID,
		"source_node_name":     sourceNode,
		"sandbox_started_at":   time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339),
		"vcpu":                 vcpu,
		"ram_mb":               int64(512),
		"total_disk_size_mb":   int64(1024),
		"kernel_version":       "vmlinux-6.1.158",
		"firecracker_version":  "v1.10.1",
		"envd_version":         "v0.3.0",
		"secure":               false,
		"metadata":             map[string]string{"stub": operationID},
		"allow_internet_access": true,
		"auto_pause":           false,
	}
}

func expectStatus(resp *Response, want int, what string) error {
	if resp.Status != want {
		return fmt.Errorf("%s: got HTTP %d, want %d; body: %s", what, resp.Status, want, truncate(resp.Body))
	}
	return nil
}

func truncate(b []byte) string {
	const max = 500
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}

func stubNode(action, sandboxID string) {
	fmt.Printf("    [stub] node action skipped: %s sandbox=%s (would call cri-multiplex admin.sock / delete Pod)\n", action, sandboxID)
}

// setBuildStatus posts the terminal status of a build.
func (s *state) setBuildStatus(buildID, operationID, status string, respErr *string) (*Response, error) {
	body := map[string]any{
		"operation_id": operationID,
		"status":       status,
	}
	if respErr != nil {
		body["error"] = *respErr
	}
	return s.client.post("/internal/builds/"+buildID+"/status", body)
}

// ---- Scenario 1: auth ----

func (s *state) scenarioAuth() error {
	noToken := ""
	wrongToken := "wrong-admin-token"
	path := "/internal/sandboxes/" + pauseSandboxID + "/snapshot-state"

	resp, err := s.client.do(http.MethodGet, path, nil, &noToken)
	if err != nil {
		return err
	}
	if err := expectStatus(resp, http.StatusUnauthorized, "GET snapshot-state without token"); err != nil {
		return err
	}

	resp, err = s.client.do(http.MethodGet, path, nil, &wrongToken)
	if err != nil {
		return err
	}
	if err := expectStatus(resp, http.StatusUnauthorized, "GET snapshot-state with wrong token"); err != nil {
		return err
	}

	// sanity: valid token passes auth
	resp, err = s.client.get(path)
	if err != nil {
		return err
	}
	if err := expectStatus(resp, http.StatusOK, "GET snapshot-state with valid token"); err != nil {
		return err
	}

	fmt.Println("    no token -> 401, wrong token -> 401, valid token -> 200")
	return nil
}

// ---- Scenario 2: request validation ----

func (s *state) scenarioValidation() error {
	body := s.snapshotBody("op-validation-0001", "stub-node-a", 2)
	delete(body, "team_id")

	resp, err := s.client.post("/internal/sandboxes/"+pauseSandboxID+"/pause-snapshot", body)
	if err != nil {
		return err
	}
	if err := expectStatus(resp, http.StatusBadRequest, "POST pause-snapshot without team_id"); err != nil {
		return err
	}

	fmt.Println("    missing team_id -> 400")
	return nil
}

// ---- Scenario 3: pause full flow ----

func (s *state) scenarioPauseFlow() error {
	body := s.snapshotBody("op-pause-0001", "stub-node-a", 2)

	resp, err := s.client.post("/internal/sandboxes/"+pauseSandboxID+"/pause-snapshot", body)
	if err != nil {
		return err
	}
	if err := expectStatus(resp, http.StatusOK, "POST pause-snapshot"); err != nil {
		return err
	}

	var parsed pauseSnapshotResponse
	if err := resp.decode(&parsed); err != nil {
		return fmt.Errorf("decode pause-snapshot response: %w", err)
	}
	if parsed.TemplateID == "" || parsed.BuildID == "" {
		return fmt.Errorf("pause-snapshot response has empty template_id/build_id: %s", truncate(resp.Body))
	}
	s.pauseTemplateID = parsed.TemplateID
	s.pauseBuildID1 = parsed.BuildID

	stubNode("Pause", pauseSandboxID)

	resp, err = s.setBuildStatus(s.pauseBuildID1, "op-pause-0001", "success", nil)
	if err != nil {
		return err
	}
	if err := expectStatus(resp, http.StatusOK, "POST builds status success"); err != nil {
		return err
	}

	// snapshot-state: paused with matching ids
	resp, err = s.client.get("/internal/sandboxes/" + pauseSandboxID + "/snapshot-state")
	if err != nil {
		return err
	}
	if err := expectStatus(resp, http.StatusOK, "GET snapshot-state"); err != nil {
		return err
	}
	var st snapshotStateResponse
	if err := resp.decode(&st); err != nil {
		return fmt.Errorf("decode snapshot-state: %w", err)
	}
	if !st.Paused {
		return fmt.Errorf("snapshot-state paused=false, want true; body: %s", truncate(resp.Body))
	}
	if st.TemplateID == nil || *st.TemplateID != s.pauseTemplateID {
		return fmt.Errorf("snapshot-state template_id=%v, want %s", st.TemplateID, s.pauseTemplateID)
	}
	if st.BuildID == nil || *st.BuildID != s.pauseBuildID1 {
		return fmt.Errorf("snapshot-state build_id=%v, want %s", st.BuildID, s.pauseBuildID1)
	}

	// last-snapshot: full metadata echo
	resp, err = s.client.get("/internal/sandboxes/" + pauseSandboxID + "/last-snapshot")
	if err != nil {
		return err
	}
	if err := expectStatus(resp, http.StatusOK, "GET last-snapshot"); err != nil {
		return err
	}
	var rm resumeMetadata
	if err := resp.decode(&rm); err != nil {
		return fmt.Errorf("decode last-snapshot: %w", err)
	}
	if rm.TemplateID != s.pauseTemplateID {
		return fmt.Errorf("last-snapshot template_id=%s, want %s", rm.TemplateID, s.pauseTemplateID)
	}
	if rm.BuildID != s.pauseBuildID1 {
		return fmt.Errorf("last-snapshot build_id=%s, want %s", rm.BuildID, s.pauseBuildID1)
	}
	if rm.BaseTemplateID == nil || *rm.BaseTemplateID != s.baseEnv {
		return fmt.Errorf("last-snapshot base_template_id=%v, want %s", rm.BaseTemplateID, s.baseEnv)
	}
	if rm.OriginNode != "stub-node-a" {
		return fmt.Errorf("last-snapshot origin_node=%s, want stub-node-a", rm.OriginNode)
	}
	if rm.Vcpu != 2 || rm.RamMb != 512 {
		return fmt.Errorf("last-snapshot vcpu=%d ram_mb=%d, want 2/512", rm.Vcpu, rm.RamMb)
	}
	if rm.TotalDiskSizeMb == nil || *rm.TotalDiskSizeMb != 1024 {
		return fmt.Errorf("last-snapshot total_disk_size_mb=%v, want 1024", rm.TotalDiskSizeMb)
	}
	if rm.KernelVersion != "vmlinux-6.1.158" || rm.FirecrackerVersion != "v1.10.1" {
		return fmt.Errorf("last-snapshot kernel=%s fc=%s, want vmlinux-6.1.158/v1.10.1", rm.KernelVersion, rm.FirecrackerVersion)
	}
	if rm.EnvdVersion == nil || *rm.EnvdVersion != "v0.3.0" {
		return fmt.Errorf("last-snapshot envd_version=%v, want v0.3.0", rm.EnvdVersion)
	}
	if rm.EnvSecure {
		return fmt.Errorf("last-snapshot env_secure=true, want false")
	}
	if rm.Metadata["stub"] != "op-pause-0001" {
		return fmt.Errorf("last-snapshot metadata echo mismatch: %v", rm.Metadata)
	}

	fmt.Printf("    paused: template_id=%s build_id=%s, last-snapshot metadata echoed\n", s.pauseTemplateID, s.pauseBuildID1)
	return nil
}

// ---- Scenario 4: pause revive semantics ----

func (s *state) scenarioPauseRevive() error {
	body := s.snapshotBody("op-pause-0002", "stub-node-b", 4)

	resp, err := s.client.post("/internal/sandboxes/"+pauseSandboxID+"/pause-snapshot", body)
	if err != nil {
		return err
	}
	if err := expectStatus(resp, http.StatusOK, "POST pause-snapshot (revive)"); err != nil {
		return err
	}

	var parsed pauseSnapshotResponse
	if err := resp.decode(&parsed); err != nil {
		return fmt.Errorf("decode pause-snapshot revive response: %w", err)
	}
	if parsed.BuildID == "" || parsed.BuildID == s.pauseBuildID1 {
		return fmt.Errorf("revive pause-snapshot build_id=%s, want a new build (first was %s)", parsed.BuildID, s.pauseBuildID1)
	}
	s.pauseBuildID2 = parsed.BuildID

	stubNode("Pause (revive)", pauseSandboxID)

	resp, err = s.setBuildStatus(s.pauseBuildID2, "op-pause-0002", "success", nil)
	if err != nil {
		return err
	}
	if err := expectStatus(resp, http.StatusOK, "POST builds status success (revive build)"); err != nil {
		return err
	}

	fmt.Printf("    revive: new build_id=%s (old %s), template_id=%s\n", s.pauseBuildID2, s.pauseBuildID1, parsed.TemplateID)
	return nil
}

// ---- Scenario 5: build status idempotency / conflict ----

func (s *state) scenarioBuildStatus() error {
	// repeat same terminal value -> idempotent 200
	resp, err := s.setBuildStatus(s.pauseBuildID2, "op-pause-0002", "success", nil)
	if err != nil {
		return err
	}
	if err := expectStatus(resp, http.StatusOK, "repeat status success"); err != nil {
		return err
	}

	// conflicting terminal value -> 409
	failMsg := "simulated build failure"
	resp, err = s.setBuildStatus(s.pauseBuildID2, "op-pause-0002", "failed", &failMsg)
	if err != nil {
		return err
	}
	if err := expectStatus(resp, http.StatusConflict, "status failed after success"); err != nil {
		return err
	}

	fmt.Println("    repeat success -> 200, success->failed -> 409")
	return nil
}

// ---- Scenario 6: checkpoint full flow ----

func (s *state) scenarioCheckpointFlow() error {
	body := s.snapshotBody("op-checkpoint-0001", "stub-node-a", 2)

	resp, err := s.client.post("/internal/sandboxes/"+checkpointSandboxID+"/snapshot-templates", body)
	if err != nil {
		return err
	}
	if err := expectStatus(resp, http.StatusOK, "POST snapshot-templates"); err != nil {
		return err
	}

	var parsed snapshotTemplateResponse
	if err := resp.decode(&parsed); err != nil {
		return fmt.Errorf("decode snapshot-templates response: %w", err)
	}
	if parsed.TemplateID == "" || parsed.BuildID == "" {
		return fmt.Errorf("snapshot-templates response has empty template_id/build_id: %s", truncate(resp.Body))
	}
	wantSnapshotID := parsed.TemplateID + ":default"
	if parsed.SnapshotID != wantSnapshotID {
		return fmt.Errorf("snapshot_id=%s, want %s", parsed.SnapshotID, wantSnapshotID)
	}
	s.cpSnapshotID = parsed.SnapshotID
	s.cpTemplateID = parsed.TemplateID
	s.cpBuildID = parsed.BuildID

	stubNode("Checkpoint", checkpointSandboxID)

	resp, err = s.setBuildStatus(s.cpBuildID, "op-checkpoint-0001", "success", nil)
	if err != nil {
		return err
	}
	if err := expectStatus(resp, http.StatusOK, "POST builds status success (checkpoint build)"); err != nil {
		return err
	}

	resp, err = s.client.get("/internal/snapshots/" + s.cpSnapshotID + "/resolve?team_id=" + s.teamID)
	if err != nil {
		return err
	}
	if err := expectStatus(resp, http.StatusOK, "GET resolve checkpoint"); err != nil {
		return err
	}
	var rm resumeMetadata
	if err := resp.decode(&rm); err != nil {
		return fmt.Errorf("decode resolve response: %w", err)
	}
	if rm.TemplateID != s.cpTemplateID {
		return fmt.Errorf("resolve template_id=%s, want %s", rm.TemplateID, s.cpTemplateID)
	}
	if rm.BuildID != s.cpBuildID {
		return fmt.Errorf("resolve build_id=%s, want %s", rm.BuildID, s.cpBuildID)
	}

	fmt.Printf("    checkpoint: snapshot_id=%s build_id=%s, resolve OK\n", s.cpSnapshotID, s.cpBuildID)
	return nil
}

// ---- Scenario 7: delete checkpoint template ----

func (s *state) scenarioDeleteCheckpoint() error {
	path := "/internal/snapshot-templates/" + s.cpSnapshotID + "?team_id=" + s.teamID

	resp, err := s.client.del(path)
	if err != nil {
		return err
	}
	if err := expectStatus(resp, http.StatusNoContent, "DELETE snapshot-templates"); err != nil {
		return err
	}

	stubNode("Delete checkpoint pod", checkpointSandboxID)

	resp, err = s.client.del(path)
	if err != nil {
		return err
	}
	if err := expectStatus(resp, http.StatusNoContent, "repeat DELETE snapshot-templates"); err != nil {
		return err
	}

	resp, err = s.client.get("/internal/snapshots/" + s.cpSnapshotID + "/resolve?team_id=" + s.teamID)
	if err != nil {
		return err
	}
	if err := expectStatus(resp, http.StatusNotFound, "resolve deleted checkpoint"); err != nil {
		return err
	}

	fmt.Println("    delete -> 204, repeat -> 204, resolve after delete -> 404")
	return nil
}

// ---- Scenario 8: delete paused sandbox snapshot ----

func (s *state) scenarioDeletePaused() error {
	path := "/internal/sandboxes/" + pauseSandboxID + "/snapshot"

	resp, err := s.client.del(path)
	if err != nil {
		return err
	}
	if err := expectStatus(resp, http.StatusNoContent, "DELETE sandboxes snapshot"); err != nil {
		return err
	}

	stubNode("Delete paused sandbox pod", pauseSandboxID)

	// NOTE: implementation detail — repeat delete returns 404 (not 204) because
	// SoftDeleteSnapshotBySandboxID matches only live rows and maps no-rows to 404.
	resp, err = s.client.del(path)
	if err != nil {
		return err
	}
	if err := expectStatus(resp, http.StatusNotFound, "repeat DELETE sandboxes snapshot (impl: 404, not idempotent)"); err != nil {
		return err
	}

	// snapshot-state: no live snapshot -> 200 paused=false
	resp, err = s.client.get("/internal/sandboxes/" + pauseSandboxID + "/snapshot-state")
	if err != nil {
		return err
	}
	if err := expectStatus(resp, http.StatusOK, "GET snapshot-state after delete"); err != nil {
		return err
	}
	var st snapshotStateResponse
	if err := resp.decode(&st); err != nil {
		return fmt.Errorf("decode snapshot-state after delete: %w", err)
	}
	if st.Paused {
		return fmt.Errorf("snapshot-state paused=true after delete, want false")
	}

	resp, err = s.client.get("/internal/sandboxes/" + pauseSandboxID + "/last-snapshot")
	if err != nil {
		return err
	}
	if err := expectStatus(resp, http.StatusNotFound, "GET last-snapshot after delete"); err != nil {
		return err
	}

	fmt.Println("    delete -> 204, repeat -> 404 (impl note), snapshot-state paused=false, last-snapshot -> 404")
	return nil
}

// ---- Scenario 9: error paths ----

func (s *state) scenarioErrorPaths() error {
	// resolve a snapshot that never existed -> 404
	resp, err := s.client.get("/internal/snapshots/nonexistent-snapshot-zz/resolve?team_id=" + s.teamID)
	if err != nil {
		return err
	}
	if err := expectStatus(resp, http.StatusNotFound, "resolve nonexistent snapshot"); err != nil {
		return err
	}

	// resolve with an unknown team_id -> 4xx (impl: 404 team not found)
	unknownTeam := "22222222-2222-2222-2222-222222222222"
	resp, err = s.client.get("/internal/snapshots/" + s.baseEnv + ":default/resolve?team_id=" + unknownTeam)
	if err != nil {
		return err
	}
	if resp.Status < 400 || resp.Status >= 500 {
		return fmt.Errorf("resolve with unknown team_id: got HTTP %d, want 4xx; body: %s", resp.Status, truncate(resp.Body))
	}
	fmt.Printf("    resolve nonexistent -> 404, resolve unknown team_id -> %d\n", resp.Status)

	// DELETE base template while a live snapshot references it
	// (scenario 6's checkpoint snapshot row still has base_env_id = base env) -> 409
	resp, err = s.client.del("/internal/templates/" + s.baseEnv + "?team_id=" + s.teamID)
	if err != nil {
		return err
	}
	if err := expectStatus(resp, http.StatusConflict, "DELETE base template referenced by live snapshot"); err != nil {
		return err
	}

	fmt.Println("    DELETE base template with live snapshot refs -> 409")
	return nil
}
