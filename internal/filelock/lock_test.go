package filelock

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestExclusiveAndCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	release, err := Acquire(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_, err = Acquire(ctx, path)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lock: %v", err)
	}
	release()
	next, err := Acquire(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	next()
}
