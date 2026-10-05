package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/localstore"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/server"
	"github.com/peterretief/yggstore/internal/share"
	"github.com/peterretief/yggstore/internal/transport"
)

func TestShareWithContact(t *testing.T) {
	dir := t.TempDir()
	var list []peers.Peer
	for i := range 4 {
		srv := httptest.NewServer(server.Handler(localstore.New(t.TempDir()), server.Options{Transport: transport.Loopback{}, Allowed: map[string]bool{"127.0.0.1": true}}))
		t.Cleanup(srv.Close)
		list = append(list, peers.Peer{Name: string(rune('a' + i)), Addr: strings.TrimPrefix(srv.URL, "http://")})
	}
	peersPath := filepath.Join(dir, "peers.json")
	peers.Write(peersPath, list)
	stubs := filepath.Join(dir, "outfiles")
	c := client.New()
	m, _, _, err := files.PutReader(context.Background(), c, bytes.NewReader([]byte("quarterly figures")), "report.txt", list, files.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(stubs, 0o755)
	stub := filepath.Join(stubs, "report.txt"+files.StubExt)
	files.WriteJSON(stub, m)

	me, _ := share.LoadOrCreate(filepath.Join(dir, "me.key"))
	bob, _ := share.LoadOrCreate(filepath.Join(dir, "bob.key"))
	d := New(Config{PeersPath: peersPath, StubDir: stubs, Client: c, Identity: me, Name: "Peter",
		ContactsPath: filepath.Join(dir, "contacts.json")})
	d.poll(context.Background())
	h := d.Handler()

	do := func(target, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, post(target, strings.NewReader(body)))
		return rec
	}
	shareReq := func(to string) string {
		b, _ := json.Marshal(map[string]string{"stub": stub, "to": to, "note": "for Friday"})
		return string(b)
	}
	if rec := do("/api/share", shareReq(bob.Code())); rec.Code != http.StatusBadRequest {
		t.Fatalf("shared with a stranger: %d", rec.Code)
	}
	if rec := do("/api/contacts", `{"name": "Bob", "code": "`+bob.Code()+`"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("add contact: %d %s", rec.Code, rec.Body)
	}
	if rec := do("/api/contacts", `{"name": "Me", "code": "`+me.Code()+`"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("added own code as a contact: %d", rec.Code)
	}
	rec := do("/api/share", shareReq(bob.Code()))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Disposition"), "report.txt.ysend") {
		t.Fatalf("share: %d %s %v", rec.Code, rec.Body, rec.Header())
	}
	got, err := bob.Open(rec.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if got.From != me.Code() || got.FromName != "Peter" || got.Note != "for Friday" {
		t.Fatalf("opened %+v", got)
	}
	var out bytes.Buffer
	received, _ := files.ReadStub(stub)
	json.Unmarshal(got.Stub, &received)
	if err := files.Get(context.Background(), c, received, &out, nil); err != nil || out.String() != "quarterly figures" {
		t.Fatalf("bob could not read it: %v %q", err, out.String())
	}

	if rec := do("/api/contacts/remove?code="+bob.Code(), ""); rec.Code != http.StatusNoContent || len(d.contacts()) != 0 {
		t.Fatalf("remove contact: %d, %d left", rec.Code, len(d.contacts()))
	}
}
