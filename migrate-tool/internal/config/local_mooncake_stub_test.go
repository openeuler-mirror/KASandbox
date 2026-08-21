//go:build !mooncake || !linux || !cgo

package config

import (
	"context"
	"strings"
	"testing"
)

func TestOpenTargetStoreReportsMissingMooncakeBuild(t *testing.T) {
	_, err := OpenTargetStore(context.Background(), "mooncake://templates-test")
	if err == nil || !strings.Contains(err.Error(), "not included in this binary") {
		t.Fatalf("OpenTargetStore() error = %v", err)
	}
}
