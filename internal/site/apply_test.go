package site

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/peterretief/yggstore/internal/peers"
)

func TestApply(t *testing.T) {
	var got []Application
	web := &Web{Dir: t.TempDir(), Peers: func() []peers.Peer { return nil }, Log: t.Logf,
		Apply: func(_ context.Context, body string) error {
			var a Application
			json.Unmarshal([]byte(body), &a)
			got = append(got, a)
			return nil
		}}
	web.st = webState{Sites: map[string]*webEntry{"get.example.org": {Owner: "::1", Serving: "v1"}}}
	web.roots = map[string]string{"get.example.org": t.TempDir()}
	send := func(form url.Values, ip, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "http://get.example.org"+ApplyPath, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.RemoteAddr = ip + ":5555"
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		web.ServeHTTP(rec, r)
		return rec
	}
	ok := url.Values{"name": {"Anna"}, "email": {"Anna@Example.net"}, "note": {"I build sites"}, "_next": {"/apply/thanks/"}}
	if r := send(ok, "192.0.2.1", "https://get.example.org"); r.Code != http.StatusSeeOther || r.Header().Get("Location") != "/apply/thanks/" {
		t.Fatalf("an application: %d %v", r.Code, r.Header())
	}
	if len(got) != 1 || got[0].Name != "Anna" || got[0].Email != "Anna@Example.net" || got[0].Site != "get.example.org" || got[0].IP != "192.0.2.1" {
		t.Fatalf("passed on: %+v", got)
	}
	for _, c := range []struct {
		form   url.Values
		origin string
		want   int
	}{
		{url.Values{"name": {"Bob"}, "email": {"not an address"}}, "", http.StatusBadRequest},
		{url.Values{"name": {""}, "email": {"bob@example.net"}}, "", http.StatusBadRequest},
		{url.Values{"name": {"Bob"}, "email": {"bob@example.net"}}, "https://evil.example", http.StatusForbidden},
	} {
		if r := send(c.form, "192.0.2.2", c.origin); r.Code != c.want {
			t.Errorf("%v from %q: %d, want %d", c.form, c.origin, r.Code, c.want)
		}
	}
	// A robot's post is answered as if taken, and dropped.
	robot := url.Values{"name": {"Bot"}, "email": {"bot@example.net"}, "_gotcha": {"x"}}
	if r := send(robot, "192.0.2.3", ""); r.Code != http.StatusOK || len(got) != 1 {
		t.Fatalf("robot: %d, %d passed on", r.Code, len(got))
	}
	// Three an hour from one address.
	for i := range 3 {
		send(url.Values{"name": {"Cara"}, "email": {"cara@example.net"}}, "192.0.2.4", "")
		_ = i
	}
	if r := send(url.Values{"name": {"Cara"}, "email": {"cara@example.net"}}, "192.0.2.4", ""); r.Code != http.StatusTooManyRequests {
		t.Fatalf("a fourth from one address: %d", r.Code)
	}
	// A web node with nowhere to send applications takes none.
	web.Apply = nil
	if r := send(ok, "192.0.2.5", ""); r.Code != http.StatusNotFound {
		t.Fatalf("no admin to send to: %d", r.Code)
	}
}
