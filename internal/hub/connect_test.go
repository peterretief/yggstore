package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterretief/yggstore/internal/syncthing"
)

const annaID = "ANNAAAA-BBBBBBB-CCCCCCC-DDDDDDD-EEEEEEE-FFFFFFF-GGGGGGG-HHHHHHH"

func TestConnectFolders(t *testing.T) {
	u, fake, dir := setupUsers(t, 0)
	ctx := context.Background()
	h := New(Config{Users: u})
	folders := func(dev string) (int, string) {
		req := httptest.NewRequest("GET", "/api/connect/folders?device="+dev, nil)
		req.RemoteAddr = "192.168.1.20:5000"
		w := httptest.NewRecorder()
		h.Handler().ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	if code, body := folders(annaID); code != 200 || body != "" {
		t.Fatalf("before pairing: %d %q; want nothing", code, body)
	}
	if code, _ := folders("not-an-id"); code != http.StatusBadRequest {
		t.Fatalf("a bad device ID: %d", code)
	}

	fake.pending[annaID] = syncthing.PendingDevice{Name: "anna", Address: "192.168.1.20:22000"}
	if err := u.step(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(ctx, filepath.Join(dir, "hub.json"), func(s *HubState) error {
		s.Teams = append(s.Teams, Team{Name: "Shop Floor", Folder: TeamFolderID("Shop Floor"), Path: filepath.Join(dir, "teams", "shop"), Members: []string{"anna"}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := u.step(ctx); err != nil {
		t.Fatal(err)
	}
	code, body := folders(annaID)
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if code != 200 || len(lines) != 2 {
		t.Fatalf("%d %q; want her folder and the team's", code, body)
	}
	if f := strings.Split(lines[0], "\t"); len(f) != 3 || f[0] != "yggstore-HUBHUBH-ANNAAAA" || f[2] != "yggstore" {
		t.Fatalf("first line %q; want her own folder, in ~/yggstore", lines[0])
	}
	if f := strings.Split(lines[1], "\t"); len(f) != 3 || f[0] != "yggstore-team-shop-floor" || f[2] != "yggstore-teams/Shop-Floor" {
		t.Fatalf("second line %q; want the team, in ~/yggstore-teams/Shop-Floor", lines[1])
	}
	other := strings.Replace(annaID, "ANNAAAA", "OTHERRR", 1)
	if _, body := folders(other); body != "" {
		t.Fatalf("a device the box hasn't taken is told %q", body)
	}
}

func TestConnectScript(t *testing.T) {
	for _, s := range []string{connectSh, connectPs1} {
		out := ConnectScript(s, "http://pi.local:7490", "pi", annaID, []string{"tcp://192.168.1.5:22000", "dynamic"})
		if strings.Contains(out, "@HUB@") || strings.Contains(out, "@BOX") || strings.Contains(out, "@ADDRS@") {
			t.Fatal("a placeholder is left in the script")
		}
		for _, want := range []string{"http://pi.local:7490", annaID, "tcp://192.168.1.5:22000 dynamic"} {
			if !strings.Contains(out, want) {
				t.Fatalf("the script has no %q", want)
			}
		}
	}
}
