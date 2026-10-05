package gateway

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestTrialEndsReadOnlyThenClosed(t *testing.T) {
	tg := startGroup(t, true, true, true)
	g, srv, cu, dir := startGateway(t, tg)
	cs := OpenCustomers(filepath.Join(dir, "customers.json"))
	cs.Update(cu.ID, func(c *Customer) { c.TrialEnds = time.Now().Add(time.Hour).Unix() })

	do(t, signedRequest(t, cu, "PUT", srv.URL+"/bkt", nil, nil))
	if st, b := do(t, signedRequest(t, cu, "PUT", srv.URL+"/bkt/a", make([]byte, 5000), nil)); st != 200 {
		t.Fatalf("put during trial: %d %s", st, b)
	}

	// The trial ends: reading and deleting still work, writing doesn't.
	cs.Update(cu.ID, func(c *Customer) { c.TrialEnds = time.Now().Add(-time.Hour).Unix() })
	if st, b := do(t, signedRequest(t, cu, "PUT", srv.URL+"/bkt/b", make([]byte, 10), nil)); st != 403 || !strings.Contains(b, "free trial ended") {
		t.Fatalf("put after trial: %d %s", st, b)
	}
	if st, b := do(t, signedRequest(t, cu, "GET", srv.URL+"/bkt/a", nil, nil)); st != 200 || len(b) != 5000 {
		t.Fatalf("get after trial: %d", st)
	}
	if st, _ := do(t, signedRequest(t, cu, "GET", srv.URL+"/bkt", nil, nil)); st != 200 {
		t.Fatalf("list after trial: %d", st)
	}

	// After the grace period, nothing works.
	cs.Update(cu.ID, func(c *Customer) { c.TrialEnds = time.Now().Add(-TrialGrace - time.Hour).Unix() })
	if st, b := do(t, signedRequest(t, cu, "GET", srv.URL+"/bkt/a", nil, nil)); st != 403 || !strings.Contains(b, "AccountProblem") {
		t.Fatalf("get after grace: %d %s", st, b)
	}

	// Closing deletes everything they stored.
	if tg.shards(t) == 0 {
		t.Fatal("no shards stored")
	}
	cs.Update(cu.ID, func(c *Customer) { c.Closed = time.Now().Unix() })
	g.chores(context.Background())
	g.Wait()
	if n := tg.shards(t); n != 0 {
		t.Fatalf("%d shards left after closing", n)
	}
	if len(g.store.buckets(cu.ID)) != 0 || g.store.usage()[cu.ID] != 0 {
		t.Fatal("closed account still has buckets or usage")
	}
}

func TestKeyLinkWorksOnce(t *testing.T) {
	tg := startGroup(t, true, true, true)
	_, srv, cu, dir := startGateway(t, tg)
	link, err := NewKeyLink(dir, cu.ID, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	get := func(method string) (int, string) {
		r, _ := http.NewRequest(method, link, nil)
		if method == "POST" {
			r.Body = io.NopCloser(strings.NewReader(""))
		}
		return do(t, r)
	}
	// Opening it (or a chat app previewing it) doesn't reveal or use it.
	for i := 0; i < 2; i++ {
		if st, b := get("GET"); st != 200 || strings.Contains(b, cu.Secret) || !strings.Contains(b, "Show my keys") {
			t.Fatalf("GET %d: %d", i, st)
		}
	}
	st, b := get("POST")
	if st != 200 || !strings.Contains(b, cu.Secret) || !strings.Contains(b, cu.AccessKey) || !strings.Contains(b, srv.URL) {
		t.Fatalf("POST: %d %s", st, b)
	}
	if st, b := get("POST"); st != http.StatusGone || strings.Contains(b, cu.Secret) {
		t.Fatalf("second POST: %d", st)
	}
	if st, _ := get("GET"); st != http.StatusGone {
		t.Fatalf("GET after use: %d", st)
	}

	// A new link replaces any unused one.
	l1, _ := NewKeyLink(dir, cu.ID, srv.URL)
	l2, _ := NewKeyLink(dir, cu.ID, srv.URL)
	r, _ := http.NewRequest("POST", l1, nil)
	if st, _ := do(t, r); st != http.StatusNotFound {
		t.Fatalf("replaced link: %d", st)
	}
	r, _ = http.NewRequest("POST", l2, nil)
	if st, b := do(t, r); st != 200 || !strings.Contains(b, cu.Secret) {
		t.Fatalf("new link: %d", st)
	}

	// Made-up links reveal nothing.
	u, _ := url.Parse(link)
	bad := srv.URL + regexp.MustCompile(`[^/]+$`).ReplaceAllString(u.Path, strings.Repeat("A", 32))
	r, _ = http.NewRequest("POST", bad, nil)
	if st, _ := do(t, r); st != http.StatusNotFound {
		t.Fatalf("made-up link: %d", st)
	}
}

func TestTrialStatus(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.Local)
	c := Customer{TrialEnds: now.Add(9*24*time.Hour + time.Hour).Unix()}
	if s := c.Status(now); !strings.HasPrefix(s, "trial, 10 days left") {
		t.Fatalf("status = %q", s)
	}
	if a, _ := c.Access(now); a != AccessFull {
		t.Fatal("trial in progress should have full access")
	}
	c.TrialEnds = now.Add(-24 * time.Hour).Unix()
	if a, why := c.Access(now); a != AccessReadOnly || !strings.Contains(why, "download your files until") {
		t.Fatalf("access = %v %q", a, why)
	}
}
