package hub

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/mail"
	"github.com/peterretief/yggstore/internal/peers"
)

// A person's website and messages, on their files page (see personsite.go):
// the site the group gave them a name for, its contact form, the messages
// that came in, and forwarding them to an email address.

//go:embed site.html
var siteHTML []byte

//go:embed mail.html
var mailHTML []byte

// SitePath is the page, beside the files page, and MailPath the person's
// mail: the messages and forwarding, with the same requests.
const (
	SitePath = FilesPath + "/site"
	MailPath = FilesPath + "/mail"
)

type sitePageState struct {
	Person   string            `json:"person"`
	Name     string            `json:"name,omitempty"` // their site and mail name, once given
	Address  string            `json:"address,omitempty"`
	Domain   string            `json:"domain,omitempty"` // the group's
	Site     *PersonSiteStatus `json:"site,omitempty"`
	Messages []mail.Summary    `json:"messages"`
	Forward  *forwardState     `json:"forward,omitempty"`
	Trial    bool              `json:"trial,omitempty"` // tighter limits on sending
	// OwnDomains are their own domains, which they may send from.
	OwnDomains []string `json:"own_domains,omitempty"`
	// Folders are their mailbox's folders.
	Folders []string `json:"folders,omitempty"`
}

type forwardState struct {
	To        string `json:"to"`
	Confirmed bool   `json:"confirmed"`
	Allowed   bool   `json:"allowed"`
	Sent      int64  `json:"sent,omitempty"` // when the code last went (unix)
}

// ownDomains are person's domains of their own, on the box at self.
func ownDomains(list []peers.Peer, self, person string) []string {
	var out []string
	for _, pn := range peers.PersonNamesOf(list, self, person) {
		if _, _, ok := peers.OwnDomain(list, pn.Name); ok {
			out = append(out, pn.Name)
		}
	}
	return out
}

func (f *filesPage) site(w http.ResponseWriter, r *http.Request, p Person) {
	ps := f.u.cfg.People
	op := r.URL.Query().Get("op")
	if op == "" && r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == MailPath {
			w.Write(mailHTML)
		} else {
			w.Write(siteHTML)
		}
		return
	}
	list, err := peers.Load(f.u.cfg.PeersPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	pn, hasAddress := peers.PersonNameOf(list, ps.cfg.Self, p.Name)
	from, named := ForwardFrom(list, ps.cfg.Self, p.Name)
	box, err := ps.Mailbox(p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	reply := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(v)
	}
	fail := func(err error) { http.Error(w, err.Error(), http.StatusBadRequest) }

	if r.Method == http.MethodGet {
		switch op {
		case "status":
			out := sitePageState{Person: p.Name, Messages: []mail.Summary{}, Domain: peers.GroupDomain(list), Trial: p.trial()}
			if named {
				out.Name = from
				if hasAddress {
					out.Name, out.Address = pn.Name, peers.MailAddress(pn.Name)
				}
				if s, ok := ps.SiteStatus(p); ok {
					out.Site = &s
				}
			}
			out.OwnDomains, out.Folders = ownDomains(list, ps.cfg.Self, p.Name), box.Folders()
			if msgs, err := box.List(); err == nil {
				out.Messages = msgs
			}
			if fw := p.Forward; fw != nil {
				out.Forward = &forwardState{To: fw.To, Confirmed: fw.Confirmed, Allowed: fw.Allowed, Sent: max(fw.SentAt, fw.Resent)}
			}
			reply(out)
		case "versions":
			vs, err := ps.Versions(p)
			if err != nil {
				fail(err)
				return
			}
			if len(vs) > 10 {
				vs = vs[:10]
			}
			reply(vs)
		case "message":
			m, err := box.Read(r.URL.Query().Get("id"))
			if errors.Is(err, mail.ErrNoMessage) {
				http.NotFound(w, r)
				return
			} else if err != nil {
				fail(err)
				return
			}
			reply(m)
		case "attachment":
			n, _ := strconv.Atoi(r.URL.Query().Get("n"))
			a, data, err := box.Attachment(r.URL.Query().Get("id"), n)
			if errors.Is(err, mail.ErrNoMessage) {
				http.NotFound(w, r)
				return
			} else if err != nil {
				fail(err)
				return
			}
			// Always a download, never a page at the person's address.
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": a.Name}))
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Content-Security-Policy", "sandbox")
			w.Header().Set("Cache-Control", "no-store")
			w.Write(data)
		default:
			http.NotFound(w, r)
		}
		return
	}
	if op == "send" {
		var req SendRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, maxSendText+16<<10)).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		group := ""
		if hasAddress {
			group = peers.MailAddress(pn.Name)
		}
		sendFrom, err := chooseFrom(req.From, group, ownDomains(list, ps.cfg.Self, p.Name))
		if err != nil {
			fail(err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
		defer cancel()
		if err := f.send(ctx, p, sendFrom, box, req); err != nil {
			fail(err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var req struct {
		On     bool     `json:"on"`
		ID     string   `json:"id"`
		To     string   `json:"to"`
		Code   string   `json:"code"`
		List   []string `json:"list"`
		Folder string   `json:"folder"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	switch op {
	case "contact":
		err = ps.SetContact(ctx, p, req.On)
	case "back":
		_, err = ps.GoBack(ctx, p, req.ID)
	case "anyway":
		err = ps.PublishAnyway(p)
	case "keep":
		err = ps.KeepServed(p)
	case "uploaders":
		err = ps.SetUploaders(ctx, p, req.List)
	case "delete":
		err = box.Delete(ctx, f.u.cfg.Client, req.ID)
	case "mkfolder":
		err = box.CreateFolder(strings.TrimSpace(req.Folder))
	case "rmfolder":
		err = box.DeleteFolder(req.Folder)
	case "move":
		err = box.Move(req.ID, req.Folder)
	case "forward":
		if !named {
			err = errors.New("you need a name in the group first: your mail is forwarded from it")
			break
		}
		err = ps.SendCode(ctx, p, from, req.To)
	case "confirm":
		_, err = Update(ctx, f.u.cfg.StatePath, func(s *HubState) error {
			if i := s.person(p.Name); i >= 0 {
				return Confirm(s.People[i].Forward, req.Code, time.Now())
			}
			return errors.New("you're no longer on this box")
		})
	case "unforward":
		_, err = Update(ctx, f.u.cfg.StatePath, func(s *HubState) error {
			if i := s.person(p.Name); i >= 0 {
				s.People[i].Forward = nil
			}
			return nil
		})
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		fail(err)
		return
	}
	if op != "delete" && op != "move" {
		f.u.cfg.Logf("hub: %s: %s on their website page", p.Name, op)
	}
	w.WriteHeader(http.StatusNoContent)
}
