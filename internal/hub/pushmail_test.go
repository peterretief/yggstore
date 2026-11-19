package hub

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/mail"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/webpush"
)

// Mail reaching someone's mailbox sends a notice to each device they asked
// for, through its push service; a device that's gone is forgotten.
func TestNewMailNotice(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "hub.json")
	nodeMail := filepath.Join(dir, "nodemail")
	os.MkdirAll(nodeMail, 0o700)
	ctx := context.Background()

	var mu sync.Mutex
	var got []string
	gone := map[string]bool{"/gone": true}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, r.URL.Path)
		if gone[r.URL.Path] {
			w.WriteHeader(http.StatusGone)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	sub := func(path string) webpush.Subscription {
		k, _ := ecdh.P256().GenerateKey(rand.Reader)
		var s webpush.Subscription
		s.Endpoint = srv.URL + path
		s.Keys.P256dh = base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes())
		s.Keys.Auth = base64.RawURLEncoding.EncodeToString(make([]byte, 16))
		return s
	}
	st, _ := Update(ctx, statePath, func(s *HubState) error {
		if _, err := s.AddPerson("Anna", filepath.Join(dir, "users")); err != nil {
			return err
		}
		s.People[0].Push = []webpush.Subscription{sub("/phone"), sub("/gone")}
		return nil
	})
	key, _ := PersonKey(statePath, st.People[0])
	peersPath := filepath.Join(dir, "peers.json")
	peers.Write(peersPath, []peers.Peer{{Name: "box", Addr: "[200::2]:7400"}})
	sealed, _ := mail.Seal(key.Code(), []byte("From: Kath <kath@localbox-004.example.internal>\r\nSubject: hi\r\n\r\nhello\r\n"))
	os.WriteFile(filepath.Join(nodeMail, "m1.sealed"), sealed, 0o600)
	os.WriteFile(filepath.Join(nodeMail, "m1.json"), []byte(`{"received":1}`), 0o600)
	os.WriteFile(filepath.Join(nodeMail, "m1.ystub"), []byte(`{}`), 0o600)

	ps := NewPeopleSites(PeopleSitesConfig{StatePath: statePath, PeersPath: peersPath, Self: "200::2", NodeMail: nodeMail,
		Dir: filepath.Join(dir, "people"), Logf: t.Logf})
	s, err := ps.pushSender()
	if err != nil {
		t.Fatal(err)
	}
	s.Client = srv.Client()
	ps.Step(ctx)

	deadline := time.Now().Add(10 * time.Second)
	for {
		st, _ := Load(statePath)
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 2 && len(st.People[0].Push) == 1 {
			if st.People[0].Push[0].Endpoint != srv.URL+"/phone" {
				t.Fatalf("kept %v", st.People[0].Push)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("notices: %v, devices now %d", got, len(st.People[0].Push))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(statePath), "push.key")); err != nil {
		t.Fatal("the box's key isn't kept")
	}
	if !strings.HasPrefix(s.PublicKey(), "B") {
		t.Fatal("not a P-256 public key")
	}
}
