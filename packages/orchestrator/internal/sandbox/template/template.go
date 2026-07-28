package template

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/build"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/metadata"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
)

type Template interface {
	Files() storage.TemplateCacheFiles
	Memfile(ctx context.Context) (block.ReadonlyDevice, error)
	Rootfs() (block.ReadonlyDevice, error)
	Disks(ctx context.Context) ([]Disk, error)
	Snapfile() (File, error)
	Metadata() (metadata.Template, error)
	Close(ctx context.Context) error
}

type Disk struct {
	Name     string
	DiffType build.DiffType
	Device   block.ReadonlyDevice
}

func RootDisk(disks []Disk) (Disk, error) {
	for _, disk := range disks {
		if disk.DiffType == build.Rootfs {
			return disk, nil
		}
	}

	return Disk{}, errors.New("rootfs disk is missing")
}

func NormalizeAndroidDisks(disks []Disk) ([]Disk, error) {
	specs := []struct {
		name     string
		diffType build.DiffType
	}{
		{storage.RootfsName, build.Rootfs},
		{storage.PersistentName, build.RootfsPersistent},
		{storage.SDCardName, build.RootfsSDCard},
	}

	if len(disks) != len(specs) {
		return nil, fmt.Errorf("Android template requires exactly %d disks, got %d", len(specs), len(disks))
	}

	byType := make(map[build.DiffType]Disk, len(disks))
	for _, disk := range disks {
		if disk.Device == nil {
			return nil, fmt.Errorf("disk %q has no block device", disk.Name)
		}
		if _, exists := byType[disk.DiffType]; exists {
			return nil, fmt.Errorf("duplicate Android disk type %q", disk.DiffType)
		}
		byType[disk.DiffType] = disk
	}

	ordered := make([]Disk, 0, len(specs))
	for _, spec := range specs {
		disk, ok := byType[spec.diffType]
		if !ok {
			return nil, fmt.Errorf("Android disk %q is missing", spec.name)
		}
		if disk.Name != spec.name {
			return nil, fmt.Errorf("Android disk type %q must be named %q, got %q", spec.diffType, spec.name, disk.Name)
		}
		ordered = append(ordered, disk)
	}

	return ordered, nil
}

func closeTemplate(ctx context.Context, t Template) (e error) {
	closable := make([]io.Closer, 0)

	memfile, err := t.Memfile(ctx)
	if err != nil {
		e = errors.Join(e, err)
	} else {
		closable = append(closable, memfile)
	}

	disks, err := t.Disks(ctx)
	if err != nil {
		e = errors.Join(e, err)
	} else {
		for _, disk := range disks {
			closable = append(closable, disk.Device)
		}
	}

	snapfile, err := t.Snapfile()
	if err != nil {
		e = errors.Join(e, err)
	} else {
		closable = append(closable, snapfile)
	}

	for _, c := range closable {
		if err := c.Close(); err != nil {
			e = errors.Join(e, err)
		}
	}

	if e != nil {
		return fmt.Errorf("error closing template: %w", e)
	}

	return nil
}

type NoopFile struct{}

func (n *NoopFile) Close() error {
	return nil
}

func (n *NoopFile) Path() string {
	return "/dev/null"
}
