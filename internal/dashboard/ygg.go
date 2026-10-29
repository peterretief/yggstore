package dashboard

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/site"
)

// The ygg button (see docs/ygg.md): a member turns it on for a website, and
// its home page gets a "Give a ygg" link to the visitor's own dashboard.
// There /ygg asks before anything is given, and the gift is an ordinary
// wallet payment to the site's owner. A page elsewhere can't give: it can
// only open /ygg, which won't be shown inside another page, and the payment
// itself needs the dashboard's own header.

//go:embed ygg.html
var yggHTML []byte

// yggFlag marks a site whose home page carries the button, so publishing a
// new version of it keeps the button.
func (d *Dashboard) yggFlag(name string) string {
	return filepath.Join(d.cfg.SitesDir, name, "ygg-button")
}

func (d *Dashboard) yggButtonOn(name string) bool {
	if d.cfg.SitesDir == "" {
		return false
	}
	_, err := os.Stat(d.yggFlag(name))
	return err == nil
}

// yggRecipient is the account a site's yggs go to: the owner of the node
// the group's list gives the site to. People on a box have no account of
// their own yet, so their sites can't be given yggs.
func yggRecipient(list []peers.Peer, name string) (string, error) {
	p, pn, ok := peers.NameHolder(list, name)
	switch {
	case !ok:
		return "", fmt.Errorf("the group's list doesn't say whose website %s is", name)
	case pn.Person != "":
		return "", fmt.Errorf("%s belongs to someone on a box, and people on a box can't be given yggs yet", name)
	case p.Operator() == "":
		return "", fmt.Errorf("%s belongs to a node with no owner, so there is no account to give to", name)
	}
	return p.Operator(), nil
}

// setYggButton puts the button on the site's home page, or takes it off.
func (d *Dashboard) setYggButton(ctx context.Context, pub *site.Publisher, name string, on bool) error {
	if d.cfg.SitesDir == "" {
		return errors.New("this dashboard keeps no websites")
	}
	if on {
		list, err := peers.Load(d.cfg.PeersPath)
		if err != nil {
			return err
		}
		if _, err := yggRecipient(list, name); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(d.yggFlag(name)), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(d.yggFlag(name), nil, 0o600); err != nil {
			return err
		}
	} else if err := os.Remove(d.yggFlag(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	verb := "added to"
	if !on {
		verb = "taken off"
	}
	return d.changeHome(ctx, pub, name, "the ygg button is switched", "ygg button "+verb, func(page string) string {
		if on {
			return site.WithYggButton(page, name)
		}
		return site.WithoutYggButton(page)
	})
}

// keepYggButton puts the button on the home page in dir, a new version of
// the site about to be published, if the site has it turned on.
func (d *Dashboard) keepYggButton(dir, name string) error {
	if !d.yggButtonOn(name) {
		return nil
	}
	index := filepath.Join(dir, "index.html")
	b, err := os.ReadFile(index)
	if err != nil {
		return nil
	}
	return os.WriteFile(index, []byte(site.WithYggButton(string(b), name)), 0o644)
}

// handleYggPage is the page that asks before giving.
func (d *Dashboard) handleYggPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	w.Write(yggHTML)
}

type yggToOut struct {
	Site    string `json:"site"`
	To      string `json:"to,omitempty"`      // the account the yggs go to
	Account string `json:"account,omitempty"` // the giver's
	Error   string `json:"error,omitempty"`
}

// handleYggTo says who a site's yggs would go to, for the page to ask.
func (d *Dashboard) handleYggTo(w http.ResponseWriter, r *http.Request) {
	out := yggToOut{Site: site.Normalise(r.URL.Query().Get("to"))}
	list, err := peers.Load(d.cfg.PeersPath)
	switch {
	case d.cfg.WalletDir == "":
		out.Error = "this dashboard has no wallet"
	case err != nil:
		out.Error = err.Error()
	case out.Site == "":
		out.Error = "no website given"
	}
	if out.Error == "" {
		out.Account = accountOf(list, d.cfg.SelfID)
		if out.To, err = yggRecipient(list, out.Site); err != nil {
			out.Error = err.Error()
		} else if out.To == out.Account {
			out.Error = out.Site + " is your own website"
		}
	}
	writeJSONResp(w, out)
}
