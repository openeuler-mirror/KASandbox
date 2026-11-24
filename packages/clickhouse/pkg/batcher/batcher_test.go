package batcher

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBatcherStartStop(t *testing.T) {
	t.Parallel()
	b, err := NewBatcher[int](func(context.Context, []int) error { return nil }, BatcherOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for range 100 {
		if err := b.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := b.Stop(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBatcherPushNotStarted(t *testing.T) {
	t.Parallel()
	b, err := NewBatcher[int](func(context.Context, []int) error { return nil }, BatcherOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ok, err := b.Push(123)
	if !errors.Is(err, ErrBatcherNotStarted) {
		t.Fatalf("expected ErrBatcherNotStarted, got %v", err)
	}
	if ok {
		t.Fatal("expected Push to fail")
	}
}

func TestBatcherStopNotStarted(t *testing.T) {
	t.Parallel()
	b, err := NewBatcher[int](func(context.Context, []int) error { return nil }, BatcherOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Stop(); !errors.Is(err, ErrBatcherNotStarted) {
		t.Fatalf("expected ErrBatcherNotStarted, got %v", err)
	}
}

func TestBatcherDoubleStop(t *testing.T) {
	t.Parallel()
	b, err := NewBatcher[int](func(context.Context, []int) error { return nil }, BatcherOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := b.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := b.Stop(); !errors.Is(err, ErrBatcherNotStarted) {
		t.Fatalf("expected ErrBatcherNotStarted, got %v", err)
	}
}

func TestBatcherDoubleStart(t *testing.T) {
	t.Parallel()
	b, err := NewBatcher[int](func(context.Context, []int) error { return nil }, BatcherOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(t.Context()); !errors.Is(err, ErrBatcherAlreadyStarted) {
		t.Fatalf("expected ErrBatcherAlreadyStarted, got %v", err)
	}
}

func TestBatcherPushStop(t *testing.T) {
	t.Parallel()
	n := 0
	b, err := NewBatcher[int](func(_ context.Context, batch []int) error {
		n += len(batch)

		return nil
	}, BatcherOptions{MaxDelay: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	for i := range 10 {
		ok, err := b.Push(i)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("cannot add item %d to batch", i)
		}
	}
	if err := b.Stop(); err != nil {
		t.Fatal(err)
	}

	if n != 10 {
		t.Fatalf("Unexpected n=%d. Expected 10", n)
	}
}

func TestBatcherPushMaxBatchSize(t *testing.T) {
	t.Parallel()
	testBatcherPushMaxBatchSize(t, 1, 100)
	testBatcherPushMaxBatchSize(t, 10, 100)
	testBatcherPushMaxBatchSize(t, 100, 100)
	testBatcherPushMaxBatchSize(t, 101, 100)
	testBatcherPushMaxBatchSize(t, 1003, 15)
	testBatcherPushMaxBatchSize(t, 1033, 17)
}

func TestBatcherPushMaxDelay(t *testing.T) {
	t.Parallel()

	const maxDelay = 200 * time.Millisecond
	const item = 42
	flushedAt := make(chan time.Time, 1)
	b, err := NewBatcher(func(_ context.Context, batch []int) error {
		if len(batch) != 1 || batch[0] != item {
			t.Errorf("flushed batch = %v, want [%d]", batch, item)
		}
		flushedAt <- time.Now()
		return nil
	}, BatcherOptions{MaxDelay: maxDelay})
	if err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now()
	if err := b.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := b.Stop(); err != nil {
			t.Error(err)
		}
	})
	if ok, err := b.Push(item); err != nil || !ok {
		t.Fatalf("Push(%d) = %t, %v", item, ok, err)
	}

	// 等待单条数据触发定时 flush，避免 Stop 关闭队列时的 flush 干扰结果。
	select {
	case at := <-flushedAt:
		if elapsed := at.Sub(startedAt); elapsed < maxDelay {
			t.Errorf("batch flushed after %s, before MaxDelay %s", elapsed, maxDelay)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("batch was not flushed by the timer")
	}
}

func TestBatcherConcurrentPush(t *testing.T) {
	t.Parallel()
	s := uint32(0)
	b, err := NewBatcher[uint32](func(_ context.Context, batch []uint32) error {
		for _, v := range batch {
			atomic.AddUint32(&s, v)
		}

		return nil
	}, BatcherOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	ss := uint32(0)
	for range 10 {
		wg.Go(func() {
			for i := range 100 {
				b.Push(uint32(i))
				time.Sleep(time.Millisecond)
				atomic.AddUint32(&ss, uint32(i))
			}
		})
	}
	wg.Wait()
	if err := b.Stop(); err != nil {
		t.Fatal(err)
	}
	if s != ss {
		t.Fatalf("Unepxected sum %d. Expecting %d", s, ss)
	}
}

func TestBatcherQueueSize(t *testing.T) {
	t.Parallel()
	ch := make(chan struct{})
	n := 0
	b, err := NewBatcher[int](func(_ context.Context, batch []int) error {
		<-ch
		n += len(batch)

		return nil
	}, BatcherOptions{
		MaxDelay:     time.Hour,
		MaxBatchSize: 3,
		QueueSize:    10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		ok, err := b.Push(i)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("cannot add item %d to batch", i)
		}
	}
	time.Sleep(time.Millisecond)
	for i := range 10 {
		ok, err := b.Push(i)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("cannot add item %d to batch", i)
		}
	}
	if b.QueueLen() != b.QueueSize {
		t.Fatalf("Unexpected queue size %d. Expecting %d", b.QueueLen(), b.QueueSize)
	}
	for range 10 {
		ok, err := b.Push(123)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Fatalf("expecting queue overflow")
		}
		time.Sleep(time.Millisecond)
	}
	close(ch)
	time.Sleep(time.Millisecond)
	for i := range 5 {
		ok, err := b.Push(i)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("cannot add item %d to batch", i)
		}
	}
	if err := b.Stop(); err != nil {
		t.Fatal(err)
	}

	if n != 18 {
		t.Fatalf("Unexpected number of items passed to batcher func: %d. Expected 18", n)
	}
}

func testBatcherPushMaxBatchSize(t *testing.T, itemsCount, batchSize int) {
	t.Helper()

	n := 0
	nn := 0
	b, err := NewBatcher[int](func(_ context.Context, batch []int) error {
		// Errorf, not Fatalf: BatcherFunc runs on the batcher goroutine where
		// FailNow would Goexit it and hang Stop().
		if len(batch) > batchSize {
			t.Errorf("Unexpected batch size=%d. Must not exceed %d. itemsCount=%d", len(batch), batchSize, itemsCount)
		}
		if len(batch) == 0 {
			t.Errorf("Empty batch. itemsCount=%d, batchSize=%d", itemsCount, batchSize)
		}
		nn += len(batch)
		n++

		return nil
	}, BatcherOptions{
		MaxDelay:     time.Hour,
		MaxBatchSize: batchSize,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	for i := range itemsCount {
		ok, err := b.Push(i)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("cannot add item %d to batch", i)
		}
	}
	if err := b.Stop(); err != nil {
		t.Fatal(err)
	}

	expectedN := (itemsCount + batchSize - 1) / batchSize
	if n != expectedN {
		t.Fatalf("Unexpected number of batcher func calls: %d. Expected %d. itemsCount=%d, batchSize=%d",
			n, expectedN, itemsCount, batchSize)
	}
	if nn != itemsCount {
		t.Fatalf("Unexpected number of items in all batches: %d. Expected %d. batchSize=%d", nn, itemsCount, batchSize)
	}
}
