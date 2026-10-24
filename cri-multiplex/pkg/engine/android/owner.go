package android

import (
	"github.com/cri-multiplex/pkg/engine"
	"github.com/cri-multiplex/pkg/engine/util"
)

// resourceOwnerRuntime 是写进 owner 标记文件的后端运行时名。
// ResourceOwner.Runtime 已改为 string，比较时统一转 string。
func androidResourceOwner(rec *AndroidSandboxRecord) util.ResourceOwner {
	return util.ResourceOwner{
		Owner:           "cri-multiplex",
		Runtime:         string(engine.EngineTypeAndroid),
		SandboxID:       rec.CRISandboxID,
		PodUID:          rec.PodUID,
		PodNamespace:    rec.Namespace,
		PodName:         rec.Name,
		BaseInstanceNum: rec.BaseInstanceNum,
		ArtifactsDir:    rec.ArtifactsDir,
	}
}

// removeOwnedAndroidPath 是 android 侧的薄包装，统一 owner 判定参数。
func removeOwnedAndroidPath(path string, owner util.ResourceOwner, cfg Config, artifacts, stateProvesOwner bool) error {
	return util.RemoveOwnedPath(path, cfg.StateDir, cfg.ArtifactsDir, owner, artifacts, stateProvesOwner)
}
