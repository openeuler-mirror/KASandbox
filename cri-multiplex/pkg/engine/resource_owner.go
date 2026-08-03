package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const resourceOwnerFile = ".cri-multiplex-owner.json"

var ErrOwnershipUnknown = errors.New("resource ownership is unknown")

type ResourceOwner struct {
	Owner           string     `json:"owner"`
	Runtime         EngineType `json:"runtime"`
	SandboxID       string     `json:"sandbox_id"`
	PodUID          string     `json:"pod_uid,omitempty"`
	PodNamespace    string     `json:"pod_namespace,omitempty"`
	PodName         string     `json:"pod_name,omitempty"`
	BaseInstanceNum int        `json:"base_instance_num,omitempty"`
	ArtifactsDir    string     `json:"artifacts_dir,omitempty"`
}

func androidResourceOwner(rec *AndroidSandboxRecord) ResourceOwner {
	return ResourceOwner{
		Owner:           "cri-multiplex",
		Runtime:         EngineTypeAndroid,
		SandboxID:       rec.CRISandboxID,
		PodUID:          rec.PodUID,
		PodNamespace:    rec.Namespace,
		PodName:         rec.Name,
		BaseInstanceNum: rec.BaseInstanceNum,
		ArtifactsDir:    rec.ArtifactsDir,
	}
}

func writeResourceOwner(path string, owner ResourceOwner) error {
	if path == "" || owner.Owner != "cri-multiplex" || owner.SandboxID == "" {
		return fmt.Errorf("invalid resource owner")
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(owner, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(path, resourceOwnerFile), data, 0o600)
}

func readResourceOwner(path string) (ResourceOwner, error) {
	data, err := os.ReadFile(filepath.Join(path, resourceOwnerFile))
	if err != nil {
		if os.IsNotExist(err) {
			return ResourceOwner{}, ErrOwnershipUnknown
		}
		return ResourceOwner{}, err
	}
	var owner ResourceOwner
	if err := json.Unmarshal(data, &owner); err != nil {
		return ResourceOwner{}, fmt.Errorf("parse owner file in %s: %w", path, err)
	}
	if owner.Owner != "cri-multiplex" || owner.Runtime == "" || owner.SandboxID == "" {
		return ResourceOwner{}, ErrOwnershipUnknown
	}
	return owner, nil
}

func ownedPathUnderRoot(path, root string) bool {
	pathAbs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return false
	}
	rootAbs, err := filepath.Abs(filepath.Clean(root))
	if err != nil || pathAbs == rootAbs {
		return false
	}
	rel, err := filepath.Rel(rootAbs, pathAbs)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func ownedArtifactsCopy(path, artifactsRoot string) bool {
	path = filepath.Clean(path)
	root := strings.TrimRight(filepath.Clean(artifactsRoot), string(os.PathSeparator))
	return path != root && strings.HasPrefix(path, root+"-") && filepath.Dir(path) == filepath.Dir(root)
}

func removeOwnedAndroidPath(path string, owner ResourceOwner, cfg AndroidConfig, artifacts, stateProvesOwner bool) error {
	if path == "" {
		return nil
	}
	if _, err := os.Lstat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	allowed := ownedPathUnderRoot(path, cfg.StateDir)
	if artifacts {
		allowed = ownedArtifactsCopy(path, cfg.ArtifactsDir)
	}
	if !allowed {
		return fmt.Errorf("refuse to remove path outside owned root: %s", path)
	}
	marker, err := readResourceOwner(path)
	if err != nil {
		if !errors.Is(err, ErrOwnershipUnknown) || !stateProvesOwner {
			return err
		}
	} else if marker.Owner != owner.Owner || marker.Runtime != owner.Runtime || marker.SandboxID != owner.SandboxID {
		return ErrOwnershipUnknown
	}
	return os.RemoveAll(path)
}
