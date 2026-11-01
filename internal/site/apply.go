package site

import (
	"encoding/json"
	"net/http"
	netmail "net/mail"
	"net/url"
	"strings"
	"time"
)

// Applying to join the group: a form on any of the group's sites (the
// page is at get.DOMAIN/apply/) posts to ApplyPath, and the web node that
// takes it passes it to the group's admins as a direct message
// (TypeApplication). The admin approves or declines it on their
// dashboard, which emails the applicant their invite. The form works like
// the contact form: same-site posts only, a _gotcha trap, and limits.
const (
	ApplyPath       = "/_yggstore/apply"
	TypeApplication = "join-apply"

	applyPerIP   = 3 // applications from one address an hour
	applyPerNode = 50
	maxApplyName = 60
	maxApplyNote = 500
)

// Application is what an applicant sent.
type Application struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Note  string `json:"note,omitempty"`
	Site  string `json:"site"` // where they applied
	IP    string `json:"ip,omitempty"`
	At    int64  `json:"at"` // unix ms
}

func (w *Web) apply(rw http.ResponseWriter, r *http.Request, site string) {
	rw.Header().Set("Cache-Control", "no-store")
	switch {
	case w.Apply == nil:
		contactReply(rw, r, http.StatusNotFound, "Applications aren't taken here.")
		return
	case r.Method != http.MethodPost:
		http.Error(rw, "An application is sent with POST.", http.StatusMethodNotAllowed)
		return
	}
	if o := r.Header.Get("Origin"); o != "" && o != "null" {
		u, err := url.Parse(o)
		if err != nil || strings.TrimPrefix(Normalise(u.Hostname()), "www.") != strings.TrimPrefix(site, "www.") {
			contactReply(rw, r, http.StatusForbidden, "The form must be on this site.")
			return
		}
	}
	fields, err := readFields(rw, r)
	if err != nil {
		contactReply(rw, r, http.StatusBadRequest, err.Error())
		return
	}
	var a Application
	var next string
	robot := false
	for _, f := range fields {
		v := strings.TrimSpace(f.value)
		switch f.name {
		case "name":
			a.Name = v
		case "email":
			a.Email = v
		case "note":
			a.Note = v
		case "_next":
			next = f.value
		case "_gotcha":
			robot = robot || v != ""
		}
	}
	addr, err := netmail.ParseAddress(a.Email)
	switch {
	case a.Name == "" || len(a.Name) > maxApplyName:
		contactReply(rw, r, http.StatusBadRequest, "Give your name, up to 60 characters.")
		return
	case err != nil || addr.Name != "" || !strings.Contains(addr.Address, "."):
		contactReply(rw, r, http.StatusBadRequest, "Give an email address your invite can be sent to.")
		return
	case len(a.Note) > maxApplyNote:
		contactReply(rw, r, http.StatusBadRequest, "Say it in up to 500 characters.")
		return
	}
	ip := visitor(r)
	if robot {
		w.logf("web: application on %s: dropped a post from %s that filled in _gotcha", site, ip)
		contactDone(rw, r, next)
		return
	}
	if !w.limit.allow("apply ip "+ip, applyPerIP, time.Hour) || !w.limit.allow("apply", applyPerNode, 24*time.Hour) {
		contactReply(rw, r, http.StatusTooManyRequests, "Too many applications; please try again later.")
		return
	}
	a.Email, a.Site, a.IP, a.At = addr.Address, site, ip, time.Now().UnixMilli()
	body, _ := json.Marshal(a)
	if err := w.Apply(r.Context(), string(body)); err != nil {
		w.logf("web: application on %s: %v", site, err)
		contactReply(rw, r, http.StatusServiceUnavailable, "Your application couldn't be passed on just now; please try again in a few minutes.")
		return
	}
	w.logf("web: application on %s from %s", site, ip)
	contactDone(rw, r, next)
}
