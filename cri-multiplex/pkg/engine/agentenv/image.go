package agentenv

import (
	"context"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// PullImage 不下载镜像；实际 rootfs 由创建 Sandbox 时选择的模板提供。
func (e *Engine) PullImage(_ context.Context, req *runtime.PullImageRequest) (*runtime.PullImageResponse, error) {
	ref := req.GetImage().GetImage()
	if err := validateVirtualImage(ref); err != nil {
		return nil, err
	}
	return &runtime.PullImageResponse{ImageRef: ref}, nil
}

// ImageStatus 对合法模板占位镜像返回稳定引用，不维护镜像缓存。
func (e *Engine) ImageStatus(_ context.Context, req *runtime.ImageStatusRequest) (*runtime.ImageStatusResponse, error) {
	ref := req.GetImage().GetImage()
	if err := validateVirtualImage(ref); err != nil {
		return nil, err
	}
	// kubelet 要求占位镜像大小非零，实际 rootfs 的大小由模板决定。
	return &runtime.ImageStatusResponse{Image: &runtime.Image{
		Id: ref, RepoTags: []string{ref}, Size_: 1,
	}}, nil
}
func (e *Engine) ListImages(context.Context, *runtime.ListImagesRequest) (*runtime.ListImagesResponse, error) {
	return &runtime.ListImagesResponse{}, nil
}
func (e *Engine) RemoveImage(context.Context, *runtime.RemoveImageRequest) (*runtime.RemoveImageResponse, error) {
	return &runtime.RemoveImageResponse{}, nil
}
func (e *Engine) ImageFsInfo(context.Context, *runtime.ImageFsInfoRequest) (*runtime.ImageFsInfoResponse, error) {
	return &runtime.ImageFsInfoResponse{}, nil
}
