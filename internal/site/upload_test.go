package site

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/mail"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/share"
)

// A JPEG's first bytes, which are what the type is told by.
var jpeg = append([]byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00"), bytes.Repeat([]byte{7}, 2000)...)

func uploadForm(t *testing.T, fields map[string]string, files map[string][2]string) (string, *bytes.Buffer) {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	for field, f := range files { // field → {file name, contents}
		w, _ := mw.CreateFormFile(field, f[0])
		w.Write([]byte(f[1]))
	}
	mw.Close()
	return mw.FormDataContentType(), &b
}

func TestUpload(t *testing.T) {
	anna, err := share.LoadOrCreate(filepath.Join(t.TempDir(), "sharing.key"))
	if err != nil {
		t.Fatal(err)
	}
	list := []peers.Peer{
		{Name: "admin", Addr: "[200::9]:7400", Admin: true, Domain: "example.org"},
		{Name: "box", Addr: "[200::1]:7400", PeopleNames: []peers.PersonName{{Name: "anna.example.org", Person: "Anna", Code: anna.Code()}}},
	}
	var got []delivered
	web := &Web{Dir: t.TempDir(), Peers: func() []peers.Peer { return list }, Log: t.Logf,
		Deliver: func(_ context.Context, node string, sealed []byte) (string, error) {
			got = append(got, delivered{node, sealed})
			return "m1", nil
		},
		Who: func(_ context.Context, token string) (map[string]any, bool) {
			switch token {
			case "tim":
				return map[string]any{"email": "Tim@Example.org", "name": "Tim", "use": "access"}, true
			case "bob":
				return map[string]any{"email": "bob@example.org", "name": "Bob", "use": "access"}, true
			}
			return nil, false
		}}
	uploaders := CleanUploaders([]string{" TIM@example.org", "not an address", "tim@example.org"})
	if len(uploaders) != 1 || uploaders[0] != "tim@example.org" {
		t.Fatalf("cleaned list: %q", uploaders)
	}
	do := func(method, token, origin, ctype string, body *bytes.Buffer, uploaders []string) (int, map[string]any) {
		t.Helper()
		if body == nil {
			body = &bytes.Buffer{}
		}
		r := httptest.NewRequest(method, "https://anna.example.org"+UploadPath, body)
		r.RemoteAddr = "127.0.0.1:5555"
		if ctype != "" {
			r.Header.Set("Content-Type", ctype)
		}
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		web.upload(rec, r, "anna.example.org", uploaders)
		var out map[string]any
		json.NewDecoder(rec.Body).Decode(&out)
		return rec.Code, out
	}

	// What the page asks first, to show its form or not.
	if _, out := do("GET", "", "", "", nil, uploaders); out["on"] != true || out["may"] != false {
		t.Fatalf("signed out: %v", out)
	}
	if _, out := do("GET", "tim", "", "", nil, uploaders); out["may"] != true {
		t.Fatalf("Tim: %v", out)
	}
	if _, out := do("GET", "tim", "", "", nil, nil); out["on"] != false {
		t.Fatalf("with nobody listed: %v", out)
	}

	photo := map[string][2]string{"file": {"../../Beach day.JPG", string(jpeg)}, "preview": {"p.jpg", string(jpeg[:500])}}
	for _, c := range []struct {
		name, token, origin string
		uploaders           []string
		files               map[string][2]string
		want                int
	}{
		{"signed out", "", "", uploaders, photo, http.StatusUnauthorized},
		{"a bad token", "forged", "", uploaders, photo, http.StatusUnauthorized},
		{"someone not listed", "bob", "", uploaders, photo, http.StatusForbidden},
		{"nobody listed", "tim", "", nil, photo, http.StatusNotFound},
		{"from another site", "tim", "https://evil.example.net", uploaders, photo, http.StatusForbidden},
		{"a page, not a photo", "tim", "", uploaders, map[string][2]string{"file": {"x.jpg", "<html><script>alert(1)</script>"}}, http.StatusBadRequest},
		{"no file", "tim", "", uploaders, nil, http.StatusBadRequest},
	} {
		ctype, body := uploadForm(t, map[string]string{"caption": "hi"}, c.files)
		if code, out := do("POST", c.token, c.origin, ctype, body, c.uploaders); code != c.want {
			t.Errorf("%s: %d %v, want %d", c.name, code, out, c.want)
		}
	}
	if len(got) != 0 {
		t.Fatalf("refused uploads were sent on: %d", len(got))
	}

	ctype, body := uploadForm(t, map[string]string{"caption": "At the  beach\nwith everyone"}, photo)
	if code, out := do("POST", "tim", "https://anna.example.org", ctype, body, uploaders); code != http.StatusOK || out["ok"] != true {
		t.Fatalf("upload: %d %v", code, out)
	}
	if len(got) != 1 || got[0].node != "200::1" {
		t.Fatalf("delivered: %v", got)
	}
	raw, err := mail.Open(anna, got[0].sealed)
	if err != nil {
		t.Fatalf("not sealed for Anna: %v", err)
	}
	up, ok, err := ReadUpload(raw)
	if !ok || err != nil {
		t.Fatalf("reading it back: %v %v", ok, err)
	}
	if up.Site != "anna.example.org" || up.Token != "tim" || up.Caption != "At the beach with everyone" || up.Name != "Beach-day.jpg" ||
		up.Type != "image/jpeg" || !bytes.Equal(up.File, jpeg) || !bytes.Equal(up.Preview, jpeg[:500]) {
		t.Fatalf("read back: %+v", Upload{Site: up.Site, Token: up.Token, Caption: up.Caption, Name: up.Name, Type: up.Type})
	}
	if !strings.Contains(string(raw), "From: \"Tim\" <tim@example.org>") {
		t.Fatalf("headers: %s", raw[:300])
	}

	// An ordinary message isn't an upload.
	if _, ok, _ := ReadUpload([]byte("From: a@b.c\r\nSubject: hi\r\n\r\nhello")); ok {
		t.Fatal("a plain message read as an upload")
	}
}

func TestSafeName(t *testing.T) {
	for in, want := range map[string]string{
		"../../etc/passwd":   "passwd.pdf",
		`C:\Users\a\b c.png`: "b-c.pdf",
		".hidden":            "file.pdf",
		"":                   "file.pdf",
		"Ünïcode foto.webp":  "Ünïcode-foto.pdf",
	} {
		if got := SafeName(in, ".pdf"); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestUploadersAnnounced(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	nodes := storageNodes(t, 6)
	b := newBus()
	pub := &Publisher{Dir: t.TempDir(), Client: client.New(), Announce: b, Log: t.Logf}
	web := &Web{Dir: t.TempDir(), Msgs: b, Peers: func() []peers.Peer { return nodes }, Client: client.New(), Log: t.Logf}
	go web.Run(ctx)
	src := t.TempDir()
	writeSite(t, src, map[string]string{"index.html": "<h1>shop</h1>"})
	if _, _, err := pub.Publish(ctx, "shop.example", src, nodes); err != nil {
		t.Fatal(err)
	}
	eventually(t, web, "shop.example", "shop")
	if err := pub.SetUploaders(ctx, "shop.example", []string{"tim@example.org", "Kath@Example.org", "nope"}); err != nil {
		t.Fatal(err)
	}
	want := "tim@example.org kath@example.org"
	if got := strings.Join(pub.Uploaders("shop.example"), " "); got != want {
		t.Fatalf("kept: %q", got)
	}
	for range 100 {
		web.mu.Lock()
		got := strings.Join(web.st.Sites["shop.example"].Uploaders, " ")
		web.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the web node didn't hear who may upload")
}
