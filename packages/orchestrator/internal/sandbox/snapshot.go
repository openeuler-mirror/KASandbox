package sandbox

import (
	"context"
	"fmt"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/build"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/template"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

type Snapshot struct {
	MemfileDiff       build.Diff
	MemfileDiffHeader *header.Header
	RootfsDiffs       map[build.DiffType]build.Diff
	RootfsDiffHeaders map[build.DiffType]*header.Header
	Snapfile          template.File
	Metafile          template.File

	cleanup *Cleanup
}

func (s *Snapshot) Upload(
	ctx context.Context,
	persistence storage.StorageProvider,
	templateFiles storage.TemplateFiles,
) error {
	var memfilePath *string
	switch r := s.MemfileDiff.(type) {
	case *build.NoDiff:
	default:
		memfileLocalPath, err := r.CachePath()
		if err != nil {
			return fmt.Errorf("error getting memfile diff path: %w", err)
		}

		memfilePath = &memfileLocalPath
	}

	rootfsPaths := make(map[build.DiffType]string)
	for diffType, r := range s.RootfsDiffs {
		if _, ok := r.(*build.NoDiff); ok {
			continue
		}
		path, err := r.CachePath()
		if err != nil {
			return fmt.Errorf("error getting %s diff path: %w", diffType, err)
		}
		rootfsPaths[diffType] = path
	}

	templateBuild := NewTemplateBuild(
		s.MemfileDiffHeader,
		s.RootfsDiffHeaders,
		persistence,
		templateFiles,
	)

	uploadErrCh := templateBuild.Upload(
		ctx,
		s.Metafile.Path(),
		s.Snapfile.Path(),
		memfilePath,
		rootfsPaths,
	)

	// Wait for the upload to finish
	uploadErr := <-uploadErrCh
	if uploadErr != nil {
		return fmt.Errorf("error uploading template build: %w", uploadErr)
	}

	return nil
}

func (s *Snapshot) Close(ctx context.Context) error {
	err := s.cleanup.Run(ctx)
	if err != nil {
		return fmt.Errorf("error cleaning up snapshot: %w", err)
	}

	return nil
}
