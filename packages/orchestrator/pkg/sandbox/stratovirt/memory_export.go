package stratovirt

import (
	"context"
	"fmt"
	"math"

	"github.com/RoaringBitmap/roaring/v2"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
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

	blocks := uint64(header.TotalBlocks(size, blockSize))
	dirty := roaring.New()
	dirty.AddRange(0, blocks)

	return &header.DiffMetadata{
		Dirty:     dirty,
		Empty:     roaring.New(),
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
	totalBlocks := uint64(header.TotalBlocks(size, blockSize))

	dirty := denseBitmapToBlockBitmap(res.Bitmap, pageSize, blockSize, totalBlocks)

	return &header.DiffMetadata{
		Dirty:     dirty,
		Empty:     roaring.New(),
		BlockSize: blockSize,
	}, nil
}

// denseBitmapToBlockBitmap converts a page-granularity bitmap (each bit = one
// pageSize) to a block-granularity roaring bitmap (each bit = one blockSize).
//  - blockSize == pageSize: 1:1
//  - blockSize >  pageSize: block dirty if any spanning page is dirty
//  - blockSize <  pageSize: all blocks a dirty page spans are marked dirty
func denseBitmapToBlockBitmap(denseBitmap []uint64, pageSize, blockSize int64, totalBlocks uint64) *roaring.Bitmap {
	dirty := roaring.New()

	for i, word := range denseBitmap {
		for bitOff := uint(0); bitOff < 64; bitOff++ {
			if word&(1<<bitOff) == 0 {
				continue
			}
			pageIdx := uint64(i)*64 + uint64(bitOff)

			if blockSize == pageSize {
				if pageIdx < totalBlocks {
					dirty.Add(uint32(pageIdx))
				}
			} else if blockSize > pageSize {
				blockIdx := pageIdx / uint64(blockSize/pageSize)
				if blockIdx < totalBlocks {
					dirty.Add(uint32(blockIdx))
				}
			} else {
				blocksPerPage := uint64(pageSize / blockSize)
				startBlock := pageIdx * blocksPerPage
				for j := uint64(0); j < blocksPerPage; j++ {
					if startBlock+j < totalBlocks {
						dirty.Add(uint32(startBlock + j))
					}
				}
			}
		}
	}

	return dirty
}

// ExportMemory snapshots the requested guest memory ranges into a block cache
// via process-vmem reads. The Firecracker-specific tuning parameters (memfd,
// background copy, dedup) have no StratoVirt equivalent and are ignored.
func (p *Process) ExportMemory(
	ctx context.Context,
	include *roaring.Bitmap,
	cachePath string,
	blockSize int64,
	_ *block.Memfd,
	_ bool,
	_ block.ReadonlyDevice,
	_ bool,
	_ bool,
	_ block.DedupBudget,
	_ *roaring.Bitmap,
	_ *utils.SetOnce[*header.DiffMetadata],
	_ bool,
	_ bool,
) (block.DiffSource, error) {
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
