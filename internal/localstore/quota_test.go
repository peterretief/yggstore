package localstore_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/peterretief/yggstore/internal/localstore"
)

func TestIndependentStoreInstancesCannotExceedSharedQuota(t *testing.T) {
	dir := t.TempDir()
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := localstore.WithQuota(dir, 10).Put([]byte{byte(i)})
			if err == nil {
				accepted.Add(1)
			} else if !errors.Is(err, localstore.ErrQuota) {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if accepted.Load() != 10 {
		t.Fatalf("accepted %d, want 10", accepted.Load())
	}
	if used, err := localstore.New(dir).Usage(); err != nil || used != 10 {
		t.Fatalf("usage %d, %v", used, err)
	}
}
