package invite

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterretief/yggstore/internal/contacts"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/share"
)

func TestJoin(t *testing.T) {
	dir := t.TempDir()
	peersPath := filepath.Join(dir, "peers.json")
	admin := peers.Peer{Name: "desktop", Addr: "[200::1]:7400", Admin: true, Owner: "peter"}
	peers.Write(peersPath, []peers.Peer{admin, {Name: "mail", Addr: "[200::5]:7400"}})
	a := Acceptor{InvitesPath: filepath.Join(dir, "invites.json"), PeersPath: peersPath, ContactsPath: filepath.Join(dir, "contacts.json")}

	inv, err := Create(a.InvitesPath, "Anna")
	if err != nil {
		t.Fatal(err)
	}
	inv.Group, inv.From, inv.Admin = "test group", "Peter", admin
	got, err := Decode("  " + inv.Encode()[:20] + "\n" + inv.Encode()[20:]) // as email might wrap it
	if err != nil || got.Token != inv.Token || got.Admin.Addr != admin.Addr {
		t.Fatalf("decode: %+v, %v", got, err)
	}

	anna, _ := share.LoadOrCreate(filepath.Join(dir, "anna.key"))
	req := Request{Token: inv.Token, Node: "anna-pc", Person: "Anna", Addr: "[200::7]:7400", SharingCode: anna.Code()}
	if _, err := a.Accept("200::8", req); err == nil {
		t.Fatal("accepted someone registering another machine's address")
	}
	if _, err := a.Accept("200::7", Request{Token: "guess", Node: "x", Addr: "[200::7]:7400"}); err != ErrBadToken {
		t.Fatalf("bad token: %v", err)
	}
	resp, err := a.Accept("200::7", req)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Peers) != 3 || resp.Node != "anna-pc" {
		t.Fatalf("response %+v", resp)
	}
	list, _ := peers.Load(peersPath)
	if p := list[2]; p.Owner != "Anna" || p.Host != "" || p.Admin {
		t.Fatalf("added %+v", p)
	}
	if c := contacts.Load(a.ContactsPath); len(c) != 1 || c[0].Name != "Anna" {
		t.Fatalf("contacts %+v", c)
	}
	if _, err := a.Accept("200::7", Request{Token: inv.Token, Node: "again", Addr: "[200::7]:7401"}); err != ErrBadToken {
		t.Fatalf("invite used twice: %v", err)
	}
	if l := List(a.InvitesPath); len(l) != 1 || l[0].UsedBy != "anna-pc" || l[0].For != "Anna" {
		t.Fatalf("invite list %+v", l)
	}

	// A second node on a machine that is already a member shares its machine.
	inv2, _ := Create(a.InvitesPath, "")
	resp, err = a.Accept("200::5", Request{Token: inv2.Token, Node: "mail", Person: "Pat", Addr: "[200::5]:7401"})
	if err != nil {
		t.Fatal(err)
	}
	list, _ = peers.Load(peersPath)
	if p := list[3]; p.Host != "mail" || p.Name == "mail" {
		t.Fatalf("same-machine node %+v (assigned %s)", p, resp.Node)
	}

	if _, err := Decode("yggjoin1:nope"); err == nil || !strings.Contains(err.Error(), "invite") {
		t.Fatalf("garbage decoded: %v", err)
	}
}
