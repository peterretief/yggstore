package dashboard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/mail"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/site"
)

// Applications to join (site.ApplyPath): web nodes pass them to the
// group's admins, whose dashboard lists them under Applications with a
// report in the admin's mailbox. Approve makes an invite and emails the
// applicant the line that joins; Decline emails a short no. Every
// application waits for the admin.

type application struct {
	ID string `json:"id"`
	site.Application
	Status  string `json:"status"` // waiting, approved, declined
	Decided int64  `json:"decided,omitempty"`
	Error   string `json:"error,omitempty"` // why the email didn't go
}

func (d *Dashboard) applicationsPath() string {
	if d.cfg.NamesPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(d.cfg.NamesPath), "applications.json")
}

// The caller holds d.namesMu.
func (d *Dashboard) loadApplications() []application {
	var out []application
	if b, err := os.ReadFile(d.applicationsPath()); err == nil {
		json.Unmarshal(b, &out)
	}
	return out
}

func (d *Dashboard) saveApplications(list []application) error {
	b, _ := json.MarshalIndent(list, "", "  ")
	return os.WriteFile(d.applicationsPath(), b, 0o600)
}

// takeApplication keeps an application a web node passed on, on an admin
// dashboard, and reports it. Called from syncNames, which holds namesMu.
func (d *Dashboard) takeApplication(ctx context.Context, list []peers.Peer, body string) {
	if !isAdmin(list, d.cfg.SelfID) || d.applicationsPath() == "" {
		return
	}
	var a site.Application
	if json.Unmarshal([]byte(body), &a) != nil || a.Name == "" || a.Email == "" {
		return
	}
	apps := d.loadApplications()
	for _, x := range apps {
		if strings.EqualFold(x.Email, a.Email) && x.Status == "waiting" {
			return // already waiting
		}
	}
	id := make([]byte, 6)
	rand.Read(id)
	apps = append(apps, application{ID: hex.EncodeToString(id), Application: a, Status: "waiting"})
	if len(apps) > 500 {
		apps = apps[len(apps)-500:]
	}
	if err := d.saveApplications(apps); err != nil {
		d.event("warn", "could not keep an application: "+err.Error())
		return
	}
	d.event("info", fmt.Sprintf("%s (%s) applied to join: Approve or Decline under Applications", a.Name, a.Email))
	note := a.Note
	if note == "" {
		note = "(nothing)"
	}
	d.mailAdmin(ctx, "Application to join: "+a.Name,
		fmt.Sprintf("%s <%s> applied to join, on %s.\r\n\r\nThey wrote: %s\r\n\r\nApprove or decline it under Applications on your dashboard's Admin page. Approving emails them their invite.\r\n",
			a.Name, a.Email, a.Site, note))
}

// mailAdmin puts a note in the admin's own mailbox.
func (d *Dashboard) mailAdmin(ctx context.Context, subject, body string) {
	box, err := d.mailBox()
	if err != nil {
		return
	}
	list, _ := peers.Load(d.cfg.PeersPath)
	domain := peers.GroupDomain(list)
	now := time.Now()
	raw := "From: yggstore <noreply@" + domain + ">\r\nTo: " + box.Address() + "\r\nSubject: " + subject +
		"\r\nDate: " + now.Format(time.RFC1123Z) + fmt.Sprintf("\r\nMessage-ID: <note.%d@%s>", now.UnixNano(), domain) +
		"\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + body
	box.Keep(ctx, d.cfg.Client, list, []byte(raw), mail.Inbox, nil, now)
}

func (d *Dashboard) handleApplications(w http.ResponseWriter, r *http.Request) {
	list, _ := peers.Load(d.cfg.PeersPath)
	if !isAdmin(list, d.cfg.SelfID) {
		http.Error(w, "only an admin dashboard takes applications", http.StatusForbidden)
		return
	}
	d.namesMu.Lock()
	apps := d.loadApplications()
	d.namesMu.Unlock()
	if apps == nil {
		apps = []application{}
	}
	sort.SliceStable(apps, func(i, j int) bool {
		if (apps[i].Status == "waiting") != (apps[j].Status == "waiting") {
			return apps[i].Status == "waiting"
		}
		return apps[i].At > apps[j].At
	})
	writeJSONResp(w, apps)
}

// handleApplicationDecide approves or declines one, and emails the applicant.
func (d *Dashboard) handleApplicationDecide(w http.ResponseWriter, r *http.Request) {
	list, _ := peers.Load(d.cfg.PeersPath)
	if !isAdmin(list, d.cfg.SelfID) {
		http.Error(w, "only an admin dashboard decides on applications", http.StatusForbidden)
		return
	}
	var req struct {
		ID      string `json:"id"`
		Approve bool   `json:"approve"`
	}
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)
	d.namesMu.Lock()
	apps := d.loadApplications()
	i := -1
	for j := range apps {
		if apps[j].ID == req.ID {
			i = j
		}
	}
	d.namesMu.Unlock()
	if i < 0 || apps[i].Status != "waiting" {
		http.Error(w, "no such application waiting", http.StatusNotFound)
		return
	}
	a := apps[i]
	group, domain := "the group", peers.GroupDomain(list)
	subject, text := "", ""
	if req.Approve {
		inv, err := d.makeInvite(a.Name + " (applied)")
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		group = inv.Group
		get := "https://get.withcare.co.za"
		if domain != "" {
			get = "https://get." + domain
		}
		subject = "Your invite to join " + group
		text = fmt.Sprintf("Hi %s,\n\nYou're welcome to join %s. Paste this line into a terminal on Linux or macOS: it installs yggstore, joins, and opens your dashboard. It's your own invite, and works once, for 7 days.\n\ncurl -fsSL %s/install.sh | sh -s -- '%s'\n\n%s\n",
			a.Name, group, get, inv.Encode(), d.me())
	} else {
		subject = "Your application to join"
		text = fmt.Sprintf("Hi %s,\n\nThank you for applying to join. The group isn't taking you on just now.\n\n%s\n", a.Name, d.me())
	}
	sendErr := d.sendMail(r.Context(), a.Email, subject, text)

	d.namesMu.Lock()
	apps = d.loadApplications()
	for j := range apps {
		if apps[j].ID == a.ID {
			apps[j].Status = map[bool]string{true: "approved", false: "declined"}[req.Approve]
			apps[j].Decided = time.Now().Unix()
			if sendErr != nil {
				apps[j].Error = sendErr.Error()
			}
		}
	}
	err := d.saveApplications(apps)
	d.namesMu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	verb := map[bool]string{true: "approved", false: "declined"}[req.Approve]
	if sendErr != nil {
		d.event("warn", fmt.Sprintf("%s's application is %s, but the email to %s didn't go: %v", a.Name, verb, a.Email, sendErr))
		writeJSONResp(w, map[string]string{"warning": "Done, but the email didn't go: " + sendErr.Error()})
		return
	}
	d.event("ok", fmt.Sprintf("%s's application is %s; emailed %s", a.Name, verb, a.Email))
	writeJSONResp(w, map[string]string{})
}

// sendMail sends a plain message from the admin's address, through the
// group's relay, and keeps it in Sent.
func (d *Dashboard) sendMail(ctx context.Context, to, subject, text string) error {
	box, err := d.mailBox()
	if err != nil {
		return err
	}
	from := box.Address()
	if from == "" {
		return fmt.Errorf("this dashboard has no address to send from")
	}
	raw, rcpts, err := mail.Draft{From: from, Name: d.me(), To: to, Subject: subject, Text: text}.Compose()
	if err != nil {
		return err
	}
	list, err := peers.Load(d.cfg.PeersPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	if err := mail.Send(ctx, d.cfg.Client, list, raw, rcpts); err != nil {
		return err
	}
	box.KeepSent(ctx, d.cfg.Client, list, raw)
	return nil
}
