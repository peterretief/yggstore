package peers

import "testing"

func TestNames(t *testing.T) {
	admin := Peer{Name: "desktop", Addr: "[200::1]:7400", Admin: true, Domain: "example.org", Names: []string{"peter.example.org"}, Code: "ys1a"}
	anna := Peer{Name: "anna", Addr: "[200::2]:7400", Names: []string{"anna.example.org"}, Code: "ys1b"}
	list := []Peer{admin, anna}
	if err := Validate(list); err != nil {
		t.Fatal(err)
	}
	if d := GroupDomain(list); d != "example.org" {
		t.Fatalf("domain %q", d)
	}
	if p, ok := MailOwner(list, "Anna@Example.org"); !ok || p.Name != "anna" {
		t.Fatalf("anna@: %v %v", p.Name, ok)
	}
	if _, ok := MailOwner(list, "bob@example.org"); ok {
		t.Fatal("bob has no name")
	}
	if p, ok := NameOwner(list, "peter.example.org."); !ok || p.Name != "desktop" {
		t.Fatal("peter.example.org")
	}
	if a := MailAddress("anna.example.org"); a != "anna@example.org" {
		t.Fatal(a)
	}

	for name, bad := range map[string][]Peer{
		"twice":         {admin, {Name: "b", Addr: "[200::3]:7400", Names: []string{"peter.example.org"}, Code: "ys1c"}},
		"no code":       {{Name: "b", Addr: "[200::3]:7400", Names: []string{"b.example.org"}}},
		"member domain": {{Name: "b", Addr: "[200::3]:7400", Domain: "example.org"}},
		"bad name":      {{Name: "b", Addr: "[200::3]:7400", Names: []string{"-b.example.org"}, Code: "ys1c"}},
		"bare domain":   {{Name: "b", Addr: "[200::3]:7400", Names: []string{"example"}, Code: "ys1c"}},
	} {
		if Validate(bad) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for _, l := range []string{"anna", "a", "anna-2", "x9"} {
		if ValidLabel(l) != nil {
			t.Errorf("%q refused", l)
		}
	}
	for _, l := range []string{"", "Anna", "-a", "a-", "a.b", "a_b", "abcdefghijklmnopqrstuvwxyz1234567"} {
		if ValidLabel(l) == nil {
			t.Errorf("%q accepted", l)
		}
	}
}
