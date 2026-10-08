package site

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/http"
	netmail "net/mail"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/mail"
)

// A site's contact form posts to ContactPath on the site itself. The web
// node writes the fields up as an email, seals it for the publisher's
// sharing code and hands it to the publisher's node, which collects it into
// its mailbox like any other mail. Nothing goes through a mail relay.
//
// Fields are listed in the form's order. Fields whose names start with _
// steer the form instead: _next is the page (on the same site) to go to
// afterwards, _subject the message's subject, and _gotcha a field people
// leave empty (hidden with CSS): anything in it marks the post as a robot's,
// which is answered as if sent and dropped. A field called email, if it
// holds an address, becomes the message's Reply-To.
const ContactPath = "/_yggstore/contact"

const (
	contactMaxBody   = 64 << 10
	contactMaxFields = 30
	contactPerIP     = 5 // messages per visitor address every contactIPWindow
	contactIPWindow  = 10 * time.Minute
	contactPerSite   = 100 // messages per site a day, per web node
)

type field struct{ name, value string }

func (w *Web) contact(rw http.ResponseWriter, r *http.Request, site, owner, code string) {
	rw.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		http.Error(rw, "The contact form is sent with POST.", http.StatusMethodNotAllowed)
		return
	}
	if code == "" || w.Deliver == nil {
		contactReply(rw, r, http.StatusNotFound, "This site's contact form is not turned on.")
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
	var next, subject string
	robot, empty := false, true
	for _, f := range fields {
		switch f.name {
		case "_next":
			next = f.value
		case "_subject":
			subject = f.value
		case "_gotcha":
			robot = robot || strings.TrimSpace(f.value) != ""
		default:
			if !strings.HasPrefix(f.name, "_") && strings.TrimSpace(f.value) != "" {
				empty = false
			}
		}
	}
	if empty {
		contactReply(rw, r, http.StatusBadRequest, "The form was empty.")
		return
	}
	ip := visitor(r)
	if robot {
		w.logf("web: contact form on %s: dropped a post from %s that filled in _gotcha", site, ip)
		contactDone(rw, r, next)
		return
	}
	if !w.limit.allow("ip "+ip, contactPerIP, contactIPWindow) {
		contactReply(rw, r, http.StatusTooManyRequests, "Too many messages from your address; please try again later.")
		return
	}
	if !w.limit.allow("site "+site, contactPerSite, 24*time.Hour) {
		contactReply(rw, r, http.StatusTooManyRequests, "This site has had too many messages today; please try again tomorrow.")
		return
	}
	raw := contactMessage(site, subject, ip, page(r, site), fields, time.Now())
	sealed, err := mail.Seal(code, raw)
	if err != nil {
		w.logf("web: contact form on %s: %v", site, err)
		contactReply(rw, r, http.StatusInternalServerError, "The message could not be sent.")
		return
	}
	id, err := w.Deliver(r.Context(), owner, sealed)
	if err != nil {
		w.logf("web: contact form on %s: %v", site, err)
		contactReply(rw, r, http.StatusServiceUnavailable, "The message could not be sent just now; please try again in a few minutes.")
		return
	}
	w.logf("web: contact form on %s: message %s for %s", site, id, owner)
	contactDone(rw, r, next)
}

// readFields reads a form, url-encoded or multipart, in its order. Files
// are left out.
func readFields(rw http.ResponseWriter, r *http.Request) ([]field, error) {
	body := http.MaxBytesReader(rw, r.Body, contactMaxBody)
	tooBig := errors.New("the form is too large")
	var out []field
	add := func(name, value string) error {
		if len(out) == contactMaxFields {
			return errors.New("the form has too many fields")
		}
		out = append(out, field{name, strings.ReplaceAll(value, "\r\n", "\n")})
		return nil
	}
	ct, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch ct {
	case "application/x-www-form-urlencoded":
		data, err := io.ReadAll(body)
		if err != nil {
			return nil, tooBig
		}
		for _, kv := range strings.Split(string(data), "&") {
			if kv == "" {
				continue
			}
			k, v, _ := strings.Cut(kv, "=")
			k, err1 := url.QueryUnescape(k)
			v, err2 := url.QueryUnescape(v)
			if err1 != nil || err2 != nil {
				return nil, errors.New("the form could not be read")
			}
			if err := add(k, v); err != nil {
				return nil, err
			}
		}
	case "multipart/form-data":
		mr := multipart.NewReader(body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, errors.New("the form could not be read (or is too large)")
			}
			if p.FileName() != "" {
				continue
			}
			v, err := io.ReadAll(p)
			if err != nil {
				return nil, tooBig
			}
			if err := add(p.FormName(), string(v)); err != nil {
				return nil, err
			}
		}
	default:
		return nil, errors.New("send the form as a normal HTML form")
	}
	return out, nil
}

// visitor is the address the post came from: Cloudflare's header when the
// request came through the tunnel (from this machine), else the peer.
func visitor(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if cf := net.ParseIP(strings.TrimSpace(r.Header.Get("CF-Connecting-IP"))); cf != nil {
			return cf.String()
		}
	}
	return host
}

// page is the path of the page the form was on, if the browser says.
func page(r *http.Request, site string) string {
	u, err := url.Parse(r.Referer())
	if err != nil || strings.TrimPrefix(Normalise(u.Hostname()), "www.") != strings.TrimPrefix(site, "www.") {
		return ""
	}
	return u.Path
}

// oneLine makes a field fit a header.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}

func contactMessage(site, subject, ip, page string, fields []field, now time.Time) []byte {
	var replyTo, first string
	for _, f := range fields {
		switch {
		case strings.EqualFold(f.name, "email") && replyTo == "":
			if a, err := netmail.ParseAddress(strings.TrimSpace(f.value)); err == nil {
				replyTo = a.Address
			}
		case strings.EqualFold(f.name, "subject") && subject == "":
			subject = f.value
		case strings.EqualFold(f.name, "message") && first == "":
			first = f.value
		}
	}
	if subject == "" {
		subject = first
	}
	subject = oneLine(subject, 80)
	if subject == "" {
		subject = "A message"
	}
	idb := make([]byte, 12)
	rand.Read(idb)
	from := (&netmail.Address{Name: "Contact form on " + site, Address: "contact-form@" + site}).String()

	var b bytes.Buffer
	hdr := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	hdr("From", from)
	hdr("To", from)
	if replyTo != "" {
		hdr("Reply-To", replyTo)
	}
	hdr("Subject", mime.QEncoding.Encode("utf-8", "["+site+"] "+subject))
	hdr("Date", now.Format(time.RFC1123Z))
	hdr("Message-ID", "<"+hex.EncodeToString(idb)+"@"+site+">")
	hdr("X-Yggstore-Contact-Form", site+"; from "+ip)
	hdr("MIME-Version", "1.0")
	hdr("Content-Type", "text/plain; charset=utf-8")
	hdr("Content-Transfer-Encoding", "quoted-printable")
	b.WriteString("\r\n")

	var text strings.Builder
	for _, f := range fields {
		if strings.HasPrefix(f.name, "_") {
			continue
		}
		name := oneLine(f.name, 64)
		if v := strings.TrimSpace(f.value); strings.Contains(v, "\n") || len(v) > 60 {
			fmt.Fprintf(&text, "%s:\n%s\n\n", name, v)
		} else {
			fmt.Fprintf(&text, "%s: %s\n", name, v)
		}
	}
	where := "https://" + site + page
	fmt.Fprintf(&text, "\n-- \nSent with the contact form on %s from %s, %s.\n", where, ip, now.Format("2 Jan 2006 15:04 MST"))
	qp := quotedprintable.NewWriter(&b)
	sc := bufio.NewScanner(strings.NewReader(text.String()))
	sc.Buffer(nil, contactMaxBody)
	for sc.Scan() {
		qp.Write([]byte(sc.Text()))
		qp.Write([]byte("\r\n"))
	}
	qp.Close()
	return b.Bytes()
}

func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

// contactDone answers a message sent: the page _next names on the same
// site, or a short thank-you.
func contactDone(rw http.ResponseWriter, r *http.Request, next string) {
	if wantsJSON(r) {
		rw.Header().Set("Content-Type", "application/json")
		json.NewEncoder(rw).Encode(map[string]any{"ok": true})
		return
	}
	if u, err := url.Parse(next); err == nil && strings.HasPrefix(next, "/") && !strings.HasPrefix(next, "//") &&
		!strings.HasPrefix(next, "/\\") && u.Host == "" && u.Scheme == "" {
		http.Redirect(rw, r, next, http.StatusSeeOther)
		return
	}
	contactReply(rw, r, http.StatusOK, "Thank you, your message was sent.")
}

func contactReply(rw http.ResponseWriter, r *http.Request, code int, text string) {
	if wantsJSON(r) {
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(code)
		body := map[string]any{"ok": code == http.StatusOK}
		if code != http.StatusOK {
			body["error"] = text
		}
		json.NewEncoder(rw).Encode(body)
		return
	}
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.WriteHeader(code)
	fmt.Fprintf(rw, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<title>Contact</title><body style="font-family:sans-serif;max-width:32em;margin:3em auto;padding:0 1em">
<p>%s</p><p><a href="/">Back to the site</a></p>`, html.EscapeString(text))
}

// limiter counts recent events per key.
type limiter struct {
	mu   sync.Mutex
	seen map[string][]time.Time
}

func (l *limiter) allow(key string, n int, per time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if l.seen == nil || len(l.seen) > 10000 {
		l.seen = map[string][]time.Time{}
	}
	recent := l.seen[key][:0]
	for _, t := range l.seen[key] {
		if now.Sub(t) < per {
			recent = append(recent, t)
		}
	}
	if len(recent) >= n {
		l.seen[key] = recent
		return false
	}
	l.seen[key] = append(recent, now)
	return true
}
