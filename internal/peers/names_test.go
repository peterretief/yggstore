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

func TestSiteNames(t *testing.T) {
	admin := Peer{Name: "desktop", Addr: "[200::1]:7400", Admin: true, Domain: "example.org", Code: "ys1a"}
	box := Peer{Name: "box", Addr: "[200::2]:7400", Code: "ys1b", Sites: []string{"club.example.org"},
		PeopleNames: []PersonName{
			{Name: "tim.example.org", Person: "Tim", Code: "ys1t"},
			{Name: "shop.example.org", Person: "Tim", Code: "ys1t", Site: true},
			{Name: "shop2.example.org", Person: "Tim", Code: "ys1t", Site: true},
		}}
	list := []Peer{admin, box}
	if err := Validate(list); err != nil {
		t.Fatal(err)
	}
	if pn, ok := PersonNameOf(list, "200::2", "tim"); !ok || pn.Name != "tim.example.org" {
		t.Fatalf("Tim's address: %+v", pn)
	}
	if all := PersonNamesOf(list, "200::2", "Tim"); len(all) != 3 || all[0].Name != "tim.example.org" {
		t.Fatalf("Tim's names: %+v", all)
	}
	for _, site := range []string{"shop.example.org", "club.example.org"} {
		if p, pn, ok := NameHolder(list, site); !ok || p.Name != "box" || !pn.Site {
			t.Fatalf("%s: %v %+v", site, ok, pn)
		}
		if keeper, given, _ := SiteKeeper(list, site); !given || keeper.Name != "box" {
			t.Fatalf("%s is the box's to publish", site)
		}
		addr := MailAddress(site)
		if _, _, ok := MailHolder(list, addr); ok {
			t.Fatalf("%s takes mail", addr)
		}
		if p, ok := MailOwner(list, addr); !ok || p.Name != "box" {
			t.Fatalf("the box can't forward from %s", addr)
		}
	}

	for name, bad := range map[string][]Peer{
		"two addresses": {admin, {Name: "b", Addr: "[200::3]:7400", PeopleNames: []PersonName{
			{Name: "a.example.org", Person: "A", Code: "ys1x"}, {Name: "b.example.org", Person: "A", Code: "ys1x"}}}},
		"site twice": {admin, box, {Name: "c", Addr: "[200::3]:7400", Code: "ys1c", Sites: []string{"shop.example.org"}}},
		"site no code": {admin, {Name: "c", Addr: "[200::3]:7400", Sites: []string{"c.example.org"}}},
		"site no person": {admin, {Name: "c", Addr: "[200::3]:7400", PeopleNames: []PersonName{
			{Name: "c.example.org", Code: "ys1x", Site: true}}}},
	} {
		if Validate(bad) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
