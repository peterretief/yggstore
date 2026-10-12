package main

import "testing"

func TestHubControl(t *testing.T) {
	for _, c := range []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{"-listen", ":7490"}, false},
		{[]string{"-service"}, false},
		{[]string{"users"}, true},
		{[]string{"-state", "x.json", "off"}, true},
		{[]string{"--state=x.json", "remove", "anna"}, true},
		{[]string{"-state", "x.json"}, false},
	} {
		if got := hubControl(c.args); got != c.want {
			t.Errorf("%q: %v, want %v", c.args, got, c.want)
		}
	}
}
