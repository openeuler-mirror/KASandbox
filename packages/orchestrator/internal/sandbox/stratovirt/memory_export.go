package stratovirt

import (
	"context"
	"fmt"
	"math"

	"github.com/bits-and-blooms/bitset"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

type memoryMapping struct {
	BaseHostVirtAddr uint64 `json:"base-host-virt-addr"`
	Size             uint64 `json:"size"`
	Offset           uint64 `json:"offset"`
}

type memoryMappings struct {
	Mappings []memoryMapping `json:"mappings"`
}

func (p *Process) queryMemoryMappings(ctx context.Context) ([]memoryMapping, error) {
	var mappings memoryMappings
	if err := p.qmpClient.executeCommandWithReturn(ctx, "query-mem-mappings", nil, &mappings); err != nil {
		return nil, fmt.Errorf("query stratovirt memory mappings: %w", err)
	}

	return mappings.Mappings, nil
}

type memoryPageState struct {
	Resident []uint64 `json:"resident"`
	Empty    []uint64 `json:"empty"`
	PageSize uint64   `json:"page-size"`
}

type memoryDirtyBitmap struct {
	Bitmap   []uint64 `json:"bitmap"`
	PageSize uint64   `json:"page-size"`
}

func (p *Process) MemoryInfo(ctx context.Context, blockSize int64) (*header.DiffMetadata, error) {
	var state memoryPageState
	if err := p.qmpClient.executeCommandWithReturn(ctx, "query-mem-page-state", nil, &state); err != nil {
		return nil, fmt.Errorf("query stratovirt memory page state: %w", err)
	}
	if blockSize <= 0 || state.PageSize != uint64(blockSize) {
		return nil, fmt.Errorf("memory page size %d does not match block size %d", state.PageSize, blockSize)
	}
	return &header.DiffMetadata{Dirty: bitset.From(state.Resident), Empty: bitset.From(state.Empty), BlockSize: blockSize}, nil
}

// Without WP_ASYNC, every query returns all loaded pages, including zero pages.
func (p *Process) DirtyMemory(ctx context.Context, blockSize int64) (*header.DiffMetadata, error) {
	var state memoryDirtyBitmap
	if err := p.qmpClient.executeCommandWithReturn(ctx, "query-mem-dirty-bitmap", nil, &state); err != nil {
		return nil, fmt.Errorf("query stratovirt memory dirty bitmap: %w", err)
	}
	if blockSize <= 0 || state.PageSize != uint64(blockSize) {
		return nil, fmt.Errorf("memory page size %d does not match block size %d", state.PageSize, blockSize)
	}
	return &header.DiffMetadata{Dirty: bitset.From(state.Bitmap), Empty: bitset.New(0), BlockSize: blockSize}, nil
}

func (p *Process) ExportMemory(
	ctx context.Context,
	include *bitset.BitSet,
	cachePath string,
	blockSize int64,
) (*block.Cache, error) {
	mappings, err := p.queryMemoryMappings(ctx)
	if err != nil {
		return nil, err
	}

	var hostRanges []block.Range
	for guestRange := range block.BitsetRanges(include, blockSize) {
		guestStart := uint64(guestRange.Start)
		guestEnd := uint64(guestRange.End())

		for _, mapping := range mappings {
			mappingEnd, overflow := addUint64(mapping.Offset, mapping.Size)
			if overflow {
				return nil, fmt.Errorf("stratovirt memory mapping size overflow")
			}

			start := max(guestStart, mapping.Offset)
			end := min(guestEnd, mappingEnd)
			if start >= end {
				continue
			}

			hostStart, overflow := addUint64(mapping.BaseHostVirtAddr, start-mapping.Offset)
			if overflow || hostStart > math.MaxInt64 || end-start > math.MaxInt64 {
				return nil, fmt.Errorf("stratovirt memory mapping exceeds supported address range")
			}

			hostRanges = append(hostRanges, block.NewRange(int64(hostStart), int64(end-start)))
		}
	}

	pid, err := p.Pid()
	if err != nil {
		return nil, err
	}

	return block.NewCacheFromProcessMemory(ctx, blockSize, cachePath, pid, hostRanges)
}

func addUint64(a, b uint64) (uint64, bool) {
	sum := a + b
	return sum, sum < a
}
