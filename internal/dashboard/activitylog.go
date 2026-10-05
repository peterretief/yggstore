package dashboard

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The activity log is kept as plain text, one event per line:
//
//	2026-10-05 16:34:14 ok    restored 14.dxf (19.9 KiB) to /home/…/14.dxf in 0.0s
//
// It is rotated to activity.log.1 at maxLogBytes.

const (
	logTime     = "2006-01-02 15:04:05"
	maxLogBytes = 5 << 20
)

func (d *Dashboard) writeLog(ev Event) {
	if d.cfg.LogPath == "" {
		return
	}
	d.logMu.Lock()
	defer d.logMu.Unlock()
	if info, err := os.Stat(d.cfg.LogPath); err == nil && info.Size() > maxLogBytes {
		os.Rename(d.cfg.LogPath, d.cfg.LogPath+".1")
	}
	os.MkdirAll(filepath.Dir(d.cfg.LogPath), 0o700)
	f, err := os.OpenFile(d.cfg.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	msg := strings.ReplaceAll(ev.Msg, "\n", " ")
	fmt.Fprintf(f, "%s %-5s %s\n", time.Unix(ev.Time, 0).Format(logTime), ev.Kind, msg)
}

// readLog returns the last n events in the log, oldest first.
func readLog(path string, n int) []Event {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if len(line) < len(logTime)+2 {
			continue
		}
		t, err := time.ParseInLocation(logTime, line[:len(logTime)], time.Local)
		if err != nil {
			continue
		}
		kind, msg, _ := strings.Cut(strings.TrimLeft(line[len(logTime):], " "), " ")
		out = append(out, Event{Time: t.Unix(), Kind: kind, Msg: strings.TrimLeft(msg, " ")})
		if len(out) > 2*n {
			out = append([]Event(nil), out[len(out)-n:]...)
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}
