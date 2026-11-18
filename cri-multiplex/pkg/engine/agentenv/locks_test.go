package agentenv

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestKeyedLockSetAllowsDifferentKeysAndRejectsCanceledWait(t *testing.T) {
	var locks keyedLockSet
	releaseA, err := locks.lock(context.Background(), "a")
	if err != nil {
		t.Fatalf("lock a: %v", err)
	}
	defer releaseA()

	releaseB, err := locks.lock(context.Background(), "b")
	if err != nil {
		t.Fatalf("different key should not block: %v", err)
	}
	releaseB()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := locks.lock(ctx, "a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("same key wait error = %v, want context deadline", err)
	}
}
