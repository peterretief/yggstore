package main

import (
	"strings"
	"testing"
	"time"
)

func TestRenderScreen(t *testing.T) {
	st := screenStatus{Box: "harbourkitchen", Owner: "anna", Version: "v1", At: time.Now(), Lines: []screenLine{
		{Label: "Network", Text: "cable, 1 Gbps", OK: true, Fix: "never shown"},
		{Label: "Internet", Text: "no", Fix: "restart the router"},
	}}
	out := renderScreen(st, "")
	for _, want := range []string{"harbourkitchen", "anna's box", "cable, 1 Gbps", "restart the router", "R\033[0m Restart"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "never shown") {
		t.Error("a fix shown for a line that's ok")
	}
	if out := renderScreen(st, "shut down"); !strings.Contains(out, "Shut down the box?") || strings.Contains(out, "R\033[0m Restart") {
		t.Errorf("asking to shut down:\n%s", out)
	}
}
