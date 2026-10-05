package dashboard

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestActivityLogSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".yggstore", "activity.log")
	d := New(Config{LogPath: path})
	d.event("ok", "restored a.txt to /tmp/a.txt")
	d.event("warn", "two\nlines")
	for i := 0; i < maxEvents+5; i++ {
		d.event("info", "filler")
	}
	d.event("bad", "the last one")

	d2 := New(Config{LogPath: path})
	if len(d2.events) != maxEvents {
		t.Fatalf("%d events after restart, want %d", len(d2.events), maxEvents)
	}
	last := d2.events[len(d2.events)-1]
	if last.Kind != "bad" || last.Msg != "the last one" || last.Time == 0 {
		t.Fatalf("last event = %+v", last)
	}
	all := readLog(path, 1000)
	if all[0].Msg != "restored a.txt to /tmp/a.txt" || all[1].Msg != "two lines" || !strings.HasPrefix(all[0].Kind, "ok") {
		t.Fatalf("first events = %+v %+v", all[0], all[1])
	}
}
