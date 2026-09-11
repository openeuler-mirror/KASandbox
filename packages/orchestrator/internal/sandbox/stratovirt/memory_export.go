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

type memDirtyBitmap struct {
	Bitmap   []uint64 `json:"bitmap"`
	PageSize uint64   `json:"page-size"`
}

func (p *Process) queryMemDirtyBitmap(ctx context.Context) (*memDirtyBitmap, error) {
	var out memDirtyBitmap
	if err := p.qmpClient.executeCommandWithReturn(ctx, "query-mem-dirty-bitmap", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func memorySize(mappings []memoryMapping) (int64, error) {
	var totalSize uint64
	for _, mapping := range mappings {
		end, overflow := addUint64(mapping.Offset, mapping.Size)
		if overflow {
			return 0, fmt.Errorf("stratovirt memory mapping size overflow")
		}
		if end > totalSize {
			totalSize = end
		}
	}
	if totalSize > math.MaxInt64 {
		return 0, fmt.Errorf("stratovirt memory snapshot is too large: %d", totalSize)
	}

	return int64(totalSize), nil
}

// MemoryInfo returns all blocks dirty, for the fresh-start path (NoopMemory).
func (p *Process) MemoryInfo(ctx context.Context, blockSize int64) (*header.DiffMetadata, error) {
	mappings, err := p.queryMemoryMappings(ctx)
	if err != nil {
		return nil, err
	}

	size, err := memorySize(mappings)
	if err != nil {
		return nil, err
	}

	blocks := uint(header.TotalBlocks(size, blockSize))
	dirty := bitset.New(blocks)
	dirty.FlipRange(0, blocks)

	return &header.DiffMetadata{
		Dirty:     dirty,
		Empty:     bitset.New(blocks),
		BlockSize: blockSize,
	}, nil
}

// DirtyMemory returns pages dirtied since the last reset via QMP
// query-mem-dirty-bitmap; the first call after UFFD restore returns all
// resident pages and enables write-protect tracking.
func (p *Process) DirtyMemory(ctx context.Context, blockSize int64) (*header.DiffMetadata, error) {
	res, err := p.queryMemDirtyBitmap(ctx)
	if err != nil {
		return nil, fmt.Errorf("query mem dirty bitmap: %w", err)
	}

	pageSize := int64(res.PageSize)
	if pageSize == 0 {
		pageSize = 4096
	}

	mappings, err := p.queryMemoryMappings(ctx)
	if err != nil {
		return nil, err
	}
	size, err := memorySize(mappings)
	if err != nil {
		return nil, err
	}
	totalBlocks := uint(header.TotalBlocks(size, blockSize))

	dirty := denseBitmapToBlockBitset(res.Bitmap, pageSize, blockSize, totalBlocks)

	return &header.DiffMetadata{
		Dirty:     dirty,
		Empty:     bitset.New(totalBlocks),
		BlockSize: blockSize,
	}, nil
}

// denseBitmapToBlockBitset converts a page-granularity bitmap (each bit = one
// pageSize) to a block-granularity BitSet (each bit = one blockSize).
//  - blockSize == pageSize: 1:1
//  - blockSize >  pageSize: block dirty if any spanning page is dirty
//  - blockSize <  pageSize: all blocks a dirty page spans are marked dirty
func denseBitmapToBlockBitset(denseBitmap []uint64, pageSize, blockSize int64, totalBlocks uint) *bitset.BitSet {
	dirty := bitset.New(totalBlocks)

	for i, word := range denseBitmap {
		for bitOff := uint(0); bitOff < 64; bitOff++ {
			if word&(1<<bitOff) == 0 {
				continue
			}
			pageIdx := uint(i)*64 + bitOff

			if blockSize == pageSize {
				if pageIdx < totalBlocks {
					dirty.Set(pageIdx)
				}
			} else if blockSize > pageSize {
				blockIdx := pageIdx / uint(blockSize/pageSize)
				if blockIdx < totalBlocks {
					dirty.Set(blockIdx)
				}
			} else {
				blocksPerPage := uint(pageSize / blockSize)
				startBlock := pageIdx * blocksPerPage
				for j := uint(0); j < blocksPerPage; j++ {
					if startBlock+j < totalBlocks {
						dirty.Set(startBlock + j)
					}
				}
			}
		}
	}

	return dirty
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
