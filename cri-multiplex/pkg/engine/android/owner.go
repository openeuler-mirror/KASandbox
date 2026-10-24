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

// removeOwnedAndroidPath 保留给 root 的薄包装；Task 10 随 android 一起搬走。
func removeOwnedAndroidPath(path string, owner util.ResourceOwner, cfg Config, artifacts, stateProvesOwner bool) error {
	return util.RemoveOwnedPath(path, cfg.StateDir, cfg.ArtifactsDir, owner, artifacts, stateProvesOwner)
}
