package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A busy node is asked again; one that stays busy is reported.
func TestDeleteRetriesBusyNode(t *testing.T) {
	busyWait = time.Millisecond
	var calls atomic.Int32
	busyFor := int32(2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= busyFor {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	if err := New().Delete(context.Background(), addr, "abc"); err != nil {
		t.Fatalf("delete after two busy answers: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("%d requests, want 3", calls.Load())
	}

	calls.Store(0)
	busyFor = 100
	if err := New().Delete(context.Background(), addr, "abc"); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("delete from a node that stays busy: %v, want a 503 error", err)
	}
	if calls.Load() != busyTries {
		t.Fatalf("%d requests, want %d", calls.Load(), busyTries)
	}
}
