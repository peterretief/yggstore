package password

import (
	"strings"
	"testing"
	"time"
)

func TestPassword(t *testing.T) {
	now := time.Now()
	pw, p := NewTemp(now)
	if len(pw) != 14 || strings.Count(pw, "-") != 2 || !p.Temp || !p.Matches(pw) || p.Matches(pw+"x") {
		t.Fatalf("temporary %q %+v", pw, p)
	}
	if !p.Live(now) || p.Live(now.Add(TempFor)) {
		t.Fatal("a temporary password works for TempFor")
	}
	c := Chosen("correct horse battery", now)
	if c.Temp || !c.Live(now.Add(10*TempFor)) || !c.Matches("correct horse battery") || c.ID() == p.ID() {
		t.Fatalf("chosen %+v", c)
	}
	for _, bad := range []string{"short", "aaaaaaaaaaaa", "1234567890", "ABCDEFGHIJKL"} {
		if OK(bad) == nil {
			t.Errorf("%q was taken", bad)
		}
	}
	if err := OK("tea at four on fridays"); err != nil {
		t.Fatal(err)
	}
}
