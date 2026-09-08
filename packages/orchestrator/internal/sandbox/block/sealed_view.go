package block

import (
	"context"
	"errors"
	"fmt"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

// SealedView is what a write layer becomes after Overlay.Seal: a read-only
// overlay of the sealed layer on top of the read path that was below it. The
// running VM keeps reading its pre-seal writes through here, while new writes
// go to the fresh layer above. Sealing k times yields a chain of k views —
// the OverlayFS lower stack, transplanted to host block level.
//
// The chain is a read path for the running sandbox only; restores read
// through a merged header instead, so chain depth never affects them.
type SealedView struct {
	device    ReadonlyDevice
	cache     *Cache
	blockSize int64
}

var _ ReadonlyDevice = (*SealedView)(nil)

func NewSealedView(device ReadonlyDevice, cache *Cache) *SealedView {
	return &SealedView{
		device:    device,
		cache:     cache,
		blockSize: device.BlockSize(),
	}
}

func (v *SealedView) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	blocks := header.BlocksOffsets(int64(len(p)), v.blockSize)

	for _, blockOff := range blocks {
		n, err := v.cache.ReadAt(p[blockOff:blockOff+v.blockSize], off+blockOff)
		if err == nil {
			continue
		}

		if !errors.As(err, &BytesNotAvailableError{}) {
			return n, fmt.Errorf("error reading from sealed layer: %w", err)
		}

		n, err = v.device.ReadAt(ctx, p[blockOff:blockOff+v.blockSize], off+blockOff)
		if err != nil {
			return n, fmt.Errorf("error reading from device below sealed layer: %w", err)
		}
	}

	return len(p), nil
}

func (v *SealedView) Size(ctx context.Context) (int64, error) {
	return v.cache.Size()
}

func (v *SealedView) BlockSize() int64 {
	return v.blockSize
}

func (v *SealedView) Slice(_ context.Context, _, _ int64) ([]byte, error) {
	return nil, fmt.Errorf("not implemented")
}

func (v *SealedView) Header() *header.Header {
	return v.device.Header()
}

// Close unmaps the sealed layer — keeping its file, which belongs to the
// checkpoint store — and closes any sealed views below it. The device at the
// bottom of the chain is not closed; its lifecycle belongs to whoever built
// it (the template cache).
func (v *SealedView) Close() error {
	err := v.cache.CloseKeepFile()

	if below, ok := v.device.(*SealedView); ok {
		err = errors.Join(err, below.Close())
	}

	return err
}
