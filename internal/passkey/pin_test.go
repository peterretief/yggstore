package passkey

import "testing"

func TestPIN(t *testing.T) {
	for _, bad := range []string{"", "12345", "111111", "123456", "987654", "4567890"} {
		if PINOK(bad) == nil {
			t.Errorf("%q passed as a PIN", bad)
		}
	}
	if err := PINOK("482913"); err != nil {
		t.Error(err)
	}
	h := PINHash("482913")
	if !PINMatches(h, "482913") || PINMatches(h, "482914") || PINMatches("", "") || h == PINHash("482913") {
		t.Error("PINHash/PINMatches")
	}
}
