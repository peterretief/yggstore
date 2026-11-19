package hub

import "testing"

func TestWithoutOurLine(t *testing.T) {
	old := "# Avahi's own\n192.168.0.9 printer.local\n"
	if got := withoutOurLine(old + localMark + "\n192.168.0.5 mail.local\n"); got != old {
		t.Fatalf("%q", got)
	}
	if got := withoutOurLine(localMark + "\n10.0.0.2 mail.local\n"); got != "\n" {
		t.Fatalf("%q", got)
	}
}
