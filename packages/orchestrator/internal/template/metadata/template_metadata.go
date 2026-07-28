package metadata

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"go.opentelemetry.io/otel"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block"
	"github.com/e2b-dev/infra/packages/shared/pkg/ioutils"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
)

const (
	CurrentVersion = 2

	DeprecatedVersion = 1
)

var tracer = otel.Tracer("github.com/e2b-dev/infra/packages/orchestrator/internal/template/metadata")

// AccessType is a compact representation of block access type for JSON serialization.
type AccessType string

const (
	AccessRead     AccessType = "r"
	AccessWrite    AccessType = "w"
	AccessPrefetch AccessType = "p"
)

// blockToAccessType maps block.AccessType to metadata.AccessType.
var blockToAccessType = map[block.AccessType]AccessType{
	block.Read:     AccessRead,
	block.Write:    AccessWrite,
	block.Prefetch: AccessPrefetch,
}

// OSType is the guest operating system family targeted by a template.
// It is set explicitly by the caller at template creation time and stored in
// template metadata; it must NOT be inferred from VMMType because a single
// VMM backend (e.g. Stratovirt) may host multiple OS families.
type OSType string

const (
	OSTypeLinux   OSType = "linux"
	OSTypeAndroid OSType = "android"
	OSTypeWindows OSType = "windows"
)

type Version struct {
	Version any `json:"version"`
}

type Context struct {
	User    string            `json:"user,omitempty"`
	WorkDir *string           `json:"workdir,omitempty"`
	EnvVars map[string]string `json:"env_vars,omitempty"`
	// OsType is the guest OS family ("linux"/"windows") the command runs on.
	// Empty means linux. It drives the shell selection at command dispatch time
	// (mirroring the SDK), so a Windows guest runs commands via powershell.exe
	// instead of /bin/bash. omitempty keeps Linux metadata byte-for-byte stable.
	OsType string `json:"os_type,omitempty"`
}

func (c Context) WithUser(user string) Context {
	c.User = user

	return c
}

func (c Context) WithOsType(osType string) Context {
	c.OsType = osType

	return c
}

type FromTemplate struct {
	Alias   string `json:"alias"`
	BuildID string `json:"build_id"`
}

type MultiDiskSpec struct {
	OS         string `json:"os"`
	Persistent string `json:"persistent"`
	SDCard     string `json:"sdcard"`
}

type Start struct {
	StartCmd string  `json:"start_command"`
	ReadyCmd string  `json:"ready_command"`
	Context  Context `json:"context"`
}

type TemplateMetadata struct {
	BuildID            string `json:"build_id"`
	KernelVersion      string `json:"kernel_version"`
	FirecrackerVersion string `json:"firecracker_version"`
	VMMType            string `json:"vmm_type,omitempty"`
	OsType             string `json:"os_type,omitempty"`
	EnvdVersion        string `json:"envd_version,omitempty"`
}

// MemoryPrefetchMapping stores block offsets that should be prefetched when starting a sandbox.
// This is used to speed up sandbox starts by proactively fetching blocks that are likely to be needed.
type MemoryPrefetchMapping struct {
	// Indices is an ordered array of block indices to prefetch, preserving the order in which blocks were accessed
	Indices []uint64 `json:"indices"`
	// AccessTypes stores the access type ("r"/"w"/"p") for each block, aligned with Indices
	AccessTypes []AccessType `json:"access_types"`
	// BlockSize is the size of each block in bytes
	BlockSize int64 `json:"block_size"`
}

// AccessTypeFromBlock converts a block.AccessType to metadata.AccessType.
func AccessTypeFromBlock(at block.AccessType) AccessType {
	return blockToAccessType[at]
}

// Count returns the number of blocks to prefetch.
func (p *MemoryPrefetchMapping) Count() int {
	if p == nil {
		return 0
	}

	return len(p.Indices)
}

type Prefetch struct {
	Memory *MemoryPrefetchMapping `json:"memory"`
}

type Template struct {
	Version  uint64           `json:"version"`
	Template TemplateMetadata `json:"template"`
	Context  Context          `json:"context"`
	Start    *Start           `json:"start,omitempty"`
	// FromImage, FromImageRaw, FromImageMultiDisk and FromTemplate are mutually exclusive.
	FromImage          *string        `json:"from_image,omitempty"`
	FromImageRaw       *string        `json:"from_image_raw,omitempty"`
	FromImageMultiDisk *MultiDiskSpec `json:"from_image_multi_disk,omitempty"`
	FromTemplate       *FromTemplate  `json:"from_template,omitempty"`
	Prefetch           *Prefetch      `json:"prefetch,omitempty"`
}

func V1TemplateVersion() Template {
	return Template{
		Version: 1,
	}
}

func (t Template) BasedOn(
	ft FromTemplate,
) Template {
	return Template{
		Version:      CurrentVersion,
		Template:     t.Template,
		Context:      t.Context,
		Start:        t.Start,
		FromTemplate: &ft,
		FromImage:    nil,
	}
}

func (t Template) NewVersionTemplate(metadata TemplateMetadata) Template {
	return Template{
		Version:            CurrentVersion,
		Template:           metadata,
		Context:            t.Context,
		Start:              t.Start,
		FromTemplate:       t.FromTemplate,
		FromImage:          t.FromImage,
		FromImageRaw:       t.FromImageRaw,
		FromImageMultiDisk: t.FromImageMultiDisk,
	}
}

func (t Template) SameVersionTemplate(metadata TemplateMetadata) Template {
	return Template{
		Version:            t.Version,
		Template:           metadata,
		Context:            t.Context,
		Start:              t.Start,
		FromTemplate:       t.FromTemplate,
		FromImage:          t.FromImage,
		FromImageRaw:       t.FromImageRaw,
		FromImageMultiDisk: t.FromImageMultiDisk,
	}
}

// WithPrefetch returns a copy of the template with the given prefetch mapping.
func (t Template) WithPrefetch(prefetch *Prefetch) Template {
	return Template{
		Version:      t.Version,
		Template:     t.Template,
		Context:      t.Context,
		Start:        t.Start,
		FromTemplate: t.FromTemplate,
		FromImage:    t.FromImage,
		Prefetch:     prefetch,
	}
}

func (t Template) ToFile(path string) error {
	mr, err := serialize(t)
	if err != nil {
		return err
	}

	err = ioutils.WriteToFileFromReader(path, mr)
	if err != nil {
		return fmt.Errorf("failed to write metadata to file: %w", err)
	}

	return nil
}

func FromFile(path string) (Template, error) {
	f, err := os.Open(path)
	if err != nil {
		return Template{}, fmt.Errorf("failed to open metadata file: %w", err)
	}
	defer f.Close()

	templateMetadata, err := deserialize(f)
	if err != nil {
		return Template{}, fmt.Errorf("failed to deserialize metadata: %w", err)
	}

	return templateMetadata, nil
}

func FromBuildID(ctx context.Context, s storage.StorageProvider, buildID string) (Template, error) {
	return fromTemplate(ctx, s, storage.TemplateFiles{
		BuildID: buildID,
	})
}

func fromTemplate(ctx context.Context, s storage.StorageProvider, files storage.TemplateFiles) (Template, error) {
	ctx, span := tracer.Start(ctx, "from template")
	defer span.End()

	obj, err := s.OpenBlob(ctx, files.StorageMetadataPath(), storage.MetadataObjectType)
	if err != nil {
		return Template{}, fmt.Errorf("error opening object for template metadata: %w", err)
	}

	data, err := storage.GetBlob(ctx, obj)
	if err != nil {
		return Template{}, fmt.Errorf("error reading template metadata from object: %w", err)
	}

	templateMetadata, err := deserialize(bytes.NewReader(data))
	if err != nil {
		return Template{}, err
	}

	return templateMetadata, nil
}

func deserialize(reader io.Reader) (Template, error) {
	// Read all data into bytes first to avoid double stream read
	data, err := io.ReadAll(reader)
	if err != nil {
		return Template{}, fmt.Errorf("error reading template metadata: %w", err)
	}

	var v Version
	err = json.Unmarshal(data, &v)
	if err != nil {
		return Template{}, fmt.Errorf("error unmarshaling template version: %w", err)
	}

	// Handle deprecated version formats gracefully
	// When any type is used, Go will unmarshal any numeric value as a float64
	if version, ok := v.Version.(float64); !ok || version <= float64(DeprecatedVersion) {
		return Template{Version: DeprecatedVersion}, nil
	}

	var templateMetadata Template
	err = json.Unmarshal(data, &templateMetadata)
	if err != nil {
		return Template{}, fmt.Errorf("error unmarshaling template metadata: %w", err)
	}

	return templateMetadata, nil
}

// serialize serializes a template to a reader for uploading.
func serialize(template Template) (io.Reader, error) {
	marshaled, err := json.Marshal(template)
	if err != nil {
		return nil, fmt.Errorf("error serializing template metadata: %w", err)
	}

	buf := bytes.NewBuffer(marshaled)

	return buf, nil
}
