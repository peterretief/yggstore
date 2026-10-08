package site

import (
	"bytes"
	"context"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/http/httptest"
	netmail "net/mail"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/mail"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/share"
)

type delivered struct {
	node   string
	sealed []byte
}

func post(h http.Handler, host, ctype, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "http://"+host+ContactPath, strings.NewReader(body))
	r.Header.Set("Content-Type", ctype)
	r.RemoteAddr = "127.0.0.1:5555"
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestContactForm(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	nodes := storageNodes(t, 6)
	b := newBus()
	pub := &Publisher{Dir: t.TempDir(), Client: client.New(), Announce: b, Log: t.Logf}
	var mu sync.Mutex
	var got []delivered
	web := &Web{Dir: t.TempDir(), Msgs: b, Peers: func() []peers.Peer { return nodes }, Client: client.New(), Log: t.Logf,
		Deliver: func(_ context.Context, node string, sealed []byte) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, delivered{node, sealed})
			return "m1", nil
		}}
	go web.Run(ctx)
	id, err := share.LoadOrCreate(filepath.Join(t.TempDir(), "sharing.key"))
	if err != nil {
		t.Fatal(err)
	}

	src := t.TempDir()
	writeSite(t, src, map[string]string{"index.html": "<h1>shop</h1>", "thanks.html": "thanks"})
	if _, _, err := pub.Publish(ctx, "shop.example", src, nodes); err != nil {
		t.Fatal(err)
	}
	eventually(t, web, "shop.example", "shop")

	if code, _ := get(t, web, http.MethodGet, "shop.example", ContactPath); code != http.StatusNotFound {
		t.Fatalf("form page while off: %d", code)
	}
	form := url.Values{"name": {"Ann"}, "email": {"ann@example.net"}, "message": {"Do you have\nblue ones?"}, "_next": {"/thanks.html"}}.Encode()
	urlenc := "application/x-www-form-urlencoded"
	if rec := post(web, "shop.example", urlenc, form, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("form while off: %d", rec.Code)
	}

	if err := pub.SetContact(ctx, "shop.example", id.Code()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec := post(web, "www.shop.example", urlenc, form, map[string]string{
			"CF-Connecting-IP": "203.0.113.7", "Referer": "https://shop.example/contact.html", "Origin": "https://www.shop.example"})
		if rec.Code == http.StatusSeeOther {
			if loc := rec.Header().Get("Location"); loc != "/thanks.html" {
				t.Fatalf("redirected to %q", loc)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("form never taken: %d %s", rec.Code, rec.Body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(got) != 1 || got[0].node != "200::1" {
		t.Fatalf("delivered %+v, want one message for the publisher's node", got)
	}
	raw, err := mail.Open(id, got[0].sealed)
	if err != nil {
		t.Fatal(err)
	}
	m, err := netmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(quotedprintable.NewReader(m.Body))
	text := strings.ReplaceAll(string(body), "\r\n", "\n")
	subject, _ := new(mime.WordDecoder).DecodeHeader(m.Header.Get("Subject"))
	for _, want := range []string{"name: Ann", "email: ann@example.net", "message:", "Do you have\nblue ones?",
		"https://shop.example/contact.html", "203.0.113.7"} {
		if !strings.Contains(text, want) {
			t.Errorf("body lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "_next") || strings.Contains(text, "thanks.html") {
		t.Errorf("body has the form's own fields:\n%s", text)
	}
	if m.Header.Get("Reply-To") != "ann@example.net" || subject != "[shop.example] Do you have blue ones?" {
		t.Errorf("Reply-To %q, Subject %q", m.Header.Get("Reply-To"), subject)
	}

	// Opened in a browser, it shows a form of its own.
	if code, page := get(t, web, http.MethodGet, "www.shop.example", ContactPath); code != 200 ||
		!strings.Contains(page, `action="`+ContactPath+`"`) || !strings.Contains(page, `name="_gotcha"`) {
		t.Fatalf("form page: %d %s", code, page)
	}

	// Multipart works too, in the form's order, and JSON is answered to fetch.
	var mp bytes.Buffer
	mw := multipart.NewWriter(&mp)
	mw.WriteField("subject", "Hours\r\nBcc: evil@example.com")
	mw.WriteField("message", "When are you open?")
	mw.Close()
	rec := post(web, "shop.example", mw.FormDataContentType(), mp.String(), map[string]string{"Accept": "application/json", "CF-Connecting-IP": "203.0.113.8"})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("multipart: %d %s", rec.Code, rec.Body)
	}
	raw, _ = mail.Open(id, got[1].sealed)
	m, _ = netmail.ReadMessage(bytes.NewReader(raw))
	if m.Header.Get("Bcc") != "" || !strings.HasPrefix(m.Header.Get("Subject"), "[shop.example] Hours Bcc:") {
		t.Fatalf("header injection: %v", m.Header)
	}

	// Robots filling in _gotcha are answered as if sent, and dropped.
	if rec := post(web, "shop.example", urlenc, form+"&_gotcha=x", map[string]string{"CF-Connecting-IP": "203.0.113.9"}); rec.Code != http.StatusSeeOther || len(got) != 2 {
		t.Fatalf("robot: %d, %d delivered", rec.Code, len(got))
	}
	// Forms on other sites, and empty forms, are refused.
	if rec := post(web, "shop.example", urlenc, form, map[string]string{"Origin": "https://evil.example"}); rec.Code != http.StatusForbidden {
		t.Fatalf("other origin: %d", rec.Code)
	}
	if rec := post(web, "shop.example", urlenc, "_next=/x", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty form: %d", rec.Code)
	}
	// _next must stay on the site.
	if rec := post(web, "shop.example", urlenc, "message=hi&_next=//evil.example/", map[string]string{"CF-Connecting-IP": "203.0.113.10"}); rec.Code != http.StatusOK {
		t.Fatalf("_next off the site: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	// One address can't send many.
	codes := map[int]int{}
	for range contactPerIP + 1 {
		codes[post(web, "shop.example", urlenc, "message=hi", map[string]string{"CF-Connecting-IP": "198.51.100.1"}).Code]++
	}
	if codes[http.StatusOK] != contactPerIP || codes[http.StatusTooManyRequests] != 1 {
		t.Fatalf("rate limit: %v", codes)
	}

	// Turning it off reaches the web node.
	if err := pub.SetContact(ctx, "shop.example", ""); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for post(web, "shop.example", urlenc, "message=hi", map[string]string{"CF-Connecting-IP": "198.51.100.2"}).Code != http.StatusNotFound {
		if time.Now().After(deadline) {
			t.Fatal("form still on")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
