package hub

import "testing"

func TestHostsWith(t *testing.T) {
	old := "# Avahi's own\n192.168.0.9 printer.local\n"
	a := hostsWith(old, "mail.local", "192.168.0.5")
	if a != old+localMark+"\n192.168.0.5 mail.local\n" {
		t.Fatalf("%q", a)
	}
	b := hostsWith(a, "mail.local", "192.168.0.7")
	if b != old+localMark+"\n192.168.0.7 mail.local\n" {
		t.Fatalf("changed: %q", b)
	}
	if c := hostsWith(b, "mail.local", ""); c != old {
		t.Fatalf("removed: %q", c)
	}
	if d := hostsWith("", "mail.local", "10.0.0.2"); d != localMark+"\n10.0.0.2 mail.local\n" {
		t.Fatalf("new: %q", d)
	}
}
