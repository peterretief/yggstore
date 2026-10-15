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

func TestPeopleNames(t *testing.T) {
	admin := Peer{Name: "desktop", Addr: "[200::1]:7400", Admin: true, Domain: "example.org"}
	box := Peer{Name: "box", Addr: "[200::2]:7400", Names: []string{"box.example.org"}, Code: "ys1box",
		PeopleNames: []PersonName{{Name: "anna.example.org", Person: "Anna", Code: "ys1anna"}}}
	list := []Peer{admin, box}
	if err := Validate(list); err != nil {
		t.Fatal(err)
	}
	if p, pn, ok := MailHolder(list, "anna@example.org"); !ok || p.Name != "box" || pn.Code != "ys1anna" || pn.Person != "Anna" {
		t.Fatalf("anna@: %v %+v %v", p.Name, pn, ok)
	}
	if _, pn, ok := MailHolder(list, "box@example.org"); !ok || pn.Code != "ys1box" || pn.Person != "" {
		t.Fatalf("box@: %+v", pn)
	}
	if owner, given, under := SiteKeeper(list, "anna.example.org"); !given || !under || owner.Name != "box" {
		t.Fatal("the box keeps anna's site")
	}
	if pn, ok := PersonNameOf(list, "200::2", "anna"); !ok || pn.Name != "anna.example.org" {
		t.Fatal("PersonNameOf")
	}
	for name, bad := range map[string]PersonName{
		"twice":     {Name: "box.example.org", Person: "Bob", Code: "ys1b"},
		"no code":   {Name: "bob.example.org", Person: "Bob"},
		"no person": {Name: "bob.example.org", Code: "ys1b"},
		"two names": {Name: "anna2.example.org", Person: "anna", Code: "ys1b"},
	} {
		b := box
		b.PeopleNames = append(append([]PersonName(nil), box.PeopleNames...), bad)
		if Validate([]Peer{admin, b}) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
