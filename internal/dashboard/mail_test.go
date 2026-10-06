package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/mail"
	"github.com/peterretief/yggstore/internal/share"
)

func TestMailbox(t *testing.T) {
	dir := t.TempDir()
	box := filepath.Join(dir, "mail")
	os.MkdirAll(box, 0o700)
	me, _ := share.LoadOrCreate(filepath.Join(dir, "me.key"))
	raw := "From: a@example.net\r\nSubject: Plans\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nHello.\r\n" +
		"--b\r\nContent-Type: text/html\r\nContent-Disposition: attachment; filename=\"page.html\"\r\n\r\n<script>alert(1)</script>\r\n--b--\r\n"
	sealed, _ := mail.Seal(me.Code(), []byte(raw))
	os.WriteFile(filepath.Join(box, "m1.sealed"), sealed, 0o600)
	os.WriteFile(filepath.Join(box, "m1.json"), []byte(`{"received": 1760000000000}`), 0o600)

	d := New(Config{PeersPath: filepath.Join(dir, "peers.json"), StubDir: dir, Client: client.New(), Identity: me, MailDir: box})
	h := d.Handler()
	get := func(target string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7480"+target, nil))
		return rec
	}
	var list struct {
		Available bool
		Messages  []mail.Summary
	}
	json.NewDecoder(get("/api/mail").Body).Decode(&list)
	if !list.Available || len(list.Messages) != 1 || list.Messages[0].Subject != "Plans" {
		t.Fatalf("list: %+v", list)
	}
	var m mail.Message
	json.NewDecoder(get("/api/mail/message?id=m1").Body).Decode(&m)
	if strings.TrimSpace(m.Text) != "Hello." || len(m.Attachments) != 1 {
		t.Fatalf("message: %+v", m)
	}
	// Parts written by strangers are only ever downloaded, never shown.
	rec := get("/api/mail/attachment?id=m1&n=0")
	if rec.Header().Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("attachment headers: %v", rec.Header())
	}
	if rec := get("/api/mail/message?id=../me"); rec.Code != http.StatusNotFound {
		t.Fatalf("path outside the mailbox: %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7480/api/mail/delete", strings.NewReader(`{"id":"m1"}`)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("delete without the header: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, post("/api/mail/delete", strings.NewReader(`{"id":"m1"}`)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(box, "m1.sealed")); !os.IsNotExist(err) {
		t.Fatal("message still there")
	}
}
