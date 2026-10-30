package password

import (
	"strings"
	"testing"
	"time"
)

func TestPassword(t *testing.T) {
	now := time.Now()
	pw, p := NewTemp(now)
	if f := strings.Split(pw, "-"); len(f) != 4 || len(f[3]) != 3 || !p.Temp || !p.Matches(pw) || p.Matches(pw+"x") {
		t.Fatalf("temporary %q %+v", pw, p)
	}
	if other, _ := NewTemp(now); other == pw {
		t.Fatal("the same temporary password twice")
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
