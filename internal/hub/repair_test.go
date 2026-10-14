package hub

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The box looks after its people's and teams' stubs, their older versions
// and recovery copies, and not what an outbox keeps of its own.
func TestStubs(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "hub.json")
	anna, team := filepath.Join(dir, "users", "anna"), filepath.Join(dir, "teams", "shop")
	for _, f := range []string{
		"users/anna/files/a.txt.ystub",
		"users/anna/received/from-bob.ystub",
		"users/anna/.yggstore/history/x/1.ystub",
		"users/anna/.yggstore/home/recovery/copy.ystub",
		"users/anna/.yggstore/home/sharing.key.ystub", // not a stored item's: not in history or recovery
		"users/anna/delete/gone.ystub",
		"users/anna/files/notes.txt",
		"teams/shop/files/price.ystub",
		"teams/shop/.yggstore/history/y/1.ystub",
	} {
		p := filepath.Join(dir, f)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("{}"), 0o644)
	}
	if _, err := Update(context.Background(), state, func(s *HubState) error {
		s.People = []Person{{Name: "anna", Path: anna}}
		s.Teams = []Team{{Name: "shop", Path: team}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	u := NewUsers(UsersConfig{StatePath: state})
	mine, renew := u.Stubs()
	var got []string
	for _, p := range mine {
		rel, _ := filepath.Rel(dir, p)
		got = append(got, filepath.ToSlash(rel))
	}
	slices.Sort(got)
	want := "teams/shop/.yggstore/history/y/1.ystub teams/shop/files/price.ystub users/anna/.yggstore/history/x/1.ystub users/anna/.yggstore/home/recovery/copy.ystub users/anna/files/a.txt.ystub users/anna/received/from-bob.ystub"
	if s := strings.Join(got, " "); s != want || len(renew) != 0 {
		t.Fatalf("looked after:\n%s\nwant\n%s\nrenew only: %v", s, want, renew)
	}
}
