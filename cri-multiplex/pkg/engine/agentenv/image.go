package agentenv

import (
	"context"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// PullImage 只登记虚拟镜像引用；实际 rootfs 由 AgentENV 模板提供。
func (e *Engine) PullImage(_ context.Context, req *runtime.PullImageRequest) (*runtime.PullImageResponse, error) {
	if req == nil || strings.TrimSpace(req.GetImage().GetImage()) == "" {
		return nil, status.Error(codes.InvalidArgument, "image reference is required")
	}
	reference := req.GetImage().GetImage()
	now := time.Now()
	record := &imageRecord{
		Reference: reference,
		ID:        agentenvImageRef(reference),
		SizeBytes: 1,
		PulledAt:  now,
	}
	e.podMu.Lock()
	previous, existed := e.images[reference]
	e.images[reference] = record
	version := e.markPodStateDirtyLocked()
	e.podMu.Unlock()
	if err := e.persistPodStateChange(version, func() {
		if existed {
			e.images[reference] = previous
		} else {
			delete(e.images, reference)
		}
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "pull virtual image: %v", err)
	}
	return &runtime.PullImageResponse{ImageRef: record.ID}, nil
}

func (e *Engine) ListImages(_ context.Context, req *runtime.ListImagesRequest) (*runtime.ListImagesResponse, error) {
	var filter string
	if req != nil && req.GetFilter() != nil {
		filter = req.GetFilter().GetImage().GetImage()
	}
	images := e.allImageRecords()
	out := make([]*runtime.Image, 0, len(images))
	for _, record := range images {
		if filter != "" && filter != record.Reference && filter != record.ID {
			continue
		}
		out = append(out, imageRecordToCRI(record))
	}
	return &runtime.ListImagesResponse{Images: out}, nil
}

func (e *Engine) ImageStatus(_ context.Context, req *runtime.ImageStatusRequest) (*runtime.ImageStatusResponse, error) {
	if req == nil || strings.TrimSpace(req.GetImage().GetImage()) == "" {
		return nil, status.Error(codes.InvalidArgument, "image reference is required")
	}
	record, ok := e.findImage(req.GetImage().GetImage())
	if !ok {
		return &runtime.ImageStatusResponse{}, nil
	}
	return &runtime.ImageStatusResponse{Image: imageRecordToCRI(record)}, nil
}

func (e *Engine) RemoveImage(_ context.Context, req *runtime.RemoveImageRequest) (*runtime.RemoveImageResponse, error) {
	if req == nil || strings.TrimSpace(req.GetImage().GetImage()) == "" {
		return nil, status.Error(codes.InvalidArgument, "image reference is required")
	}
	reference := req.GetImage().GetImage()
	e.podMu.Lock()
	key, previous, ok := e.findImageLocked(reference)
	if !ok {
		e.podMu.Unlock()
		return &runtime.RemoveImageResponse{}, nil
	}
	delete(e.images, key)
	version := e.markPodStateDirtyLocked()
	e.podMu.Unlock()
	if err := e.persistPodStateChange(version, func() {
		e.images[key] = previous
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "remove virtual image: %v", err)
	}
	return &runtime.RemoveImageResponse{}, nil
}

// ImageFsInfo 当前没有独立的节点镜像文件系统。
func (e *Engine) ImageFsInfo(context.Context, *runtime.ImageFsInfoRequest) (*runtime.ImageFsInfoResponse, error) {
	return &runtime.ImageFsInfoResponse{}, nil
}

func (e *Engine) allImageRecords() []imageRecord {
	e.podMu.RLock()
	defer e.podMu.RUnlock()
	byReference := make(map[string]imageRecord, len(e.images))
	for reference, record := range e.images {
		byReference[reference] = *record
	}
	for _, container := range e.containers {
		if container.ImageName == "" {
			continue
		}
		if _, exists := byReference[container.ImageName]; exists {
			continue
		}
		byReference[container.ImageName] = imageRecord{
			Reference: container.ImageName,
			ID:        container.ImageRef,
			SizeBytes: 1,
			PulledAt:  container.CreatedAt,
		}
	}
	out := make([]imageRecord, 0, len(byReference))
	for _, record := range byReference {
		out = append(out, record)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Reference < out[j].Reference })
	return out
}

func (e *Engine) findImage(reference string) (imageRecord, bool) {
	e.podMu.RLock()
	defer e.podMu.RUnlock()
	_, record, ok := e.findImageLocked(reference)
	if !ok {
		return imageRecord{}, false
	}
	return *record, true
}

func (e *Engine) findImageLocked(reference string) (string, *imageRecord, bool) {
	if record, ok := e.images[reference]; ok {
		return reference, record, true
	}
	for key, record := range e.images {
		if record.ID == reference {
			return key, record, true
		}
	}
	for _, container := range e.containers {
		if container.ImageName == reference || container.ImageRef == reference {
			return container.ImageName, &imageRecord{
				Reference: container.ImageName,
				ID:        container.ImageRef,
				SizeBytes: 1,
				PulledAt:  container.CreatedAt,
			}, true
		}
	}
	return "", nil, false
}

func imageRecordToCRI(record imageRecord) *runtime.Image {
	return &runtime.Image{
		Id:       record.ID,
		RepoTags: []string{record.Reference},
		Size_:    record.SizeBytes,
	}
}
