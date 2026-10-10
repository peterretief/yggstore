package dashboard

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"unicode"

	"github.com/peterretief/yggstore/internal/atomicfile"
)

// The person's own name, as people see it: on mail they send, shares and
// invites. They set it on the dashboard (kept in MePath); until then it is
// the -me the dashboard was started with, often just the machine's name.

func (d *Dashboard) me() string {
	if d.cfg.MePath != "" {
		if b, err := os.ReadFile(d.cfg.MePath); err == nil {
			if n := strings.TrimSpace(string(b)); n != "" {
				return n
			}
		}
	}
	return d.cfg.Name
}

func validMe(name string) error {
	if name == "" || len([]rune(name)) > 64 {
		return errors.New("give a name of up to 64 characters")
	}
	for _, r := range name {
		if unicode.IsControl(r) || strings.ContainsRune(`"<>\`, r) {
			return errors.New(`a name can't hold " < > \ or control characters`)
		}
	}
	return nil
}

func (d *Dashboard) handleSetMe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := strings.Join(strings.Fields(req.Name), " ")
	if err := validMe(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if d.cfg.MePath == "" {
		http.Error(w, "this dashboard has nowhere to keep your name", http.StatusServiceUnavailable)
		return
	}
	if err := atomicfile.Replace(d.cfg.MePath, []byte(name+"\n"), 0o600); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d.poke()
	w.WriteHeader(http.StatusNoContent)
}
