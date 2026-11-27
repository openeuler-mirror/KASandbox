//go:build !mooncake || !cgo

package storage

import "errors"

// newMooncakeClient fails at provider construction, so a binary without
// Mooncake support stops at startup instead of on the first read.
func newMooncakeClient() (mooncakeKV, error) {
	return nil, errors.New("STORAGE_PROVIDER=MooncakeBucket needs an orchestrator built with -tags mooncake and cgo")
}
