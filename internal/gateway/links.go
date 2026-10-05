package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A key link is a one-time web page that shows a customer their keys, so
// the organiser never has to send the secret itself. Opening the link
// shows a button; the keys appear when it is pressed, and the link then
// stops working. Link previews in chat apps and mail scanners only fetch
// the page, so they don't use it up.

const (
	linkBucket = "_keys"
	LinkTTL    = 7 * 24 * time.Hour
)

type keyLink struct {
	Customer string `json:"customer"`
	Endpoint string `json:"endpoint"`
	Created  int64  `json:"created"`
	Expires  int64  `json:"expires"`
	Used     int64  `json:"used,omitempty"`
}

func linksDir(dir string) string { return filepath.Join(dir, "links") }

func linkPath(dir, token string) string {
	h := sha256.Sum256([]byte(token))
	return filepath.Join(linksDir(dir), hex.EncodeToString(h[:])+".json")
}

// NewKeyLink makes a one-time link to customer's keys, valid for LinkTTL,
// and returns its address. Earlier unused links for the customer stop
// working. endpoint is the gateway's public address.
func NewKeyLink(dir, customer, endpoint string) (string, error) {
	endpoint = strings.TrimRight(endpoint, "/")
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		return "", errors.New("the endpoint must start with https:// (or http:// for testing)")
	}
	entries, _ := os.ReadDir(linksDir(dir))
	now := time.Now()
	for _, e := range entries {
		var l keyLink
		p := filepath.Join(linksDir(dir), e.Name())
		if readJSON(p, &l) == nil && (l.Customer == customer || now.Unix() > l.Expires+30*24*3600) {
			os.Remove(p)
		}
	}
	token := randomString(secretChars, 32)
	l := keyLink{Customer: customer, Endpoint: endpoint, Created: now.Unix(), Expires: now.Add(LinkTTL).Unix()}
	if err := writeJSON(linkPath(dir, token), l); err != nil {
		return "", err
	}
	return endpoint + "/" + linkBucket + "/" + token, nil
}

// serveKeyLink answers requests for a key link.
func (g *Gateway) serveKeyLink(w http.ResponseWriter, r *http.Request, ip, token string) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
	page := keyPage{}

	g.linkMu.Lock()
	defer g.linkMu.Unlock()
	path := linkPath(g.store.dir, token)
	var l keyLink
	err := readJSON(path, &l)
	var cu Customer
	if err == nil {
		cu, err = g.Customers.Find(l.Customer)
	}
	now := time.Now()
	switch {
	case err != nil || len(token) != 32:
		g.fails.add(ip)
		w.WriteHeader(http.StatusNotFound)
		page.Problem = "This link isn't valid. Check that you copied all of it, or ask the organiser for a new one."
	case l.Used != 0:
		w.WriteHeader(http.StatusGone)
		page.Problem = "This link was already used, on " + time.Unix(l.Used, 0).Local().Format("2 Jan 2006 at 15:04") +
			". If that wasn't you, tell the organiser straight away: they will give you new keys."
	case now.Unix() > l.Expires:
		w.WriteHeader(http.StatusGone)
		page.Problem = "This link has expired. Ask the organiser for a new one."
	case r.Method == http.MethodPost:
		l.Used = now.Unix()
		if err := writeJSON(path, l); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			page.Problem = "Something went wrong on our side; try again in a minute."
			break
		}
		g.opts.Log("key link for %s (%s) opened from %s", cu.Name, cu.ID, ip)
		page.C, page.Endpoint, page.Show = cu, l.Endpoint, true
		page.Space = quotaText(cu.QuotaBytes)
		if cu.TrialEnds != 0 {
			page.TrialEnds = day(time.Unix(cu.TrialEnds, 0))
		}
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		page.C, page.Expires = cu, day(time.Unix(l.Expires, 0))
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		page.Problem = "Open this link in a web browser."
	}
	keyTmpl.Execute(w, page)
}

type keyPage struct {
	Problem   string
	C         Customer
	Show      bool
	Endpoint  string
	Space     string
	Expires   string
	TrialEnds string
}

// QuotaText is a quota for people: "unlimited" or "5 GB".
func QuotaText(n int64) string { return quotaText(n) }

func quotaText(n int64) string {
	if n == 0 {
		return "unlimited"
	}
	return fmt.Sprintf("%g GB", float64(n)/1e9)
}

var keyTmpl = template.Must(template.New("keys").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex"><title>Your storage keys</title>
<style>
:root { --bg: #f7f6f2; --fg: #1d2421; --sub: #5d6762; --box: #fff; --line: #d9ddd8; --accent: #1f6f50; --warn: #8a5a00; color-scheme: light; }
@media (prefers-color-scheme: dark) { :root { --bg: #151917; --fg: #e4e9e6; --sub: #9aa59f; --box: #1f2522; --line: #343c38; --accent: #5cc496; --warn: #e0b050; color-scheme: dark; } }
body { margin: 0; background: var(--bg); color: var(--fg); font: 16px/1.5 system-ui, -apple-system, "Segoe UI", sans-serif; }
main { max-width: 640px; margin: 0 auto; padding: 32px 16px 48px; }
h1 { font-size: 24px; margin: 0 0 8px; text-wrap: balance; }
p { margin: 0 0 14px; }
.sub { color: var(--sub); font-size: 14px; }
.box { background: var(--box); border: 1px solid var(--line); border-radius: 8px; padding: 16px; margin: 16px 0; }
dl { display: grid; grid-template-columns: max-content 1fr; gap: 8px 16px; margin: 0; }
dt { color: var(--sub); }
dd { margin: 0; font-family: ui-monospace, Consolas, monospace; overflow-wrap: anywhere; user-select: all; }
pre { background: var(--box); border: 1px solid var(--line); border-radius: 8px; padding: 12px; overflow-x: auto; font-size: 13px; user-select: all; }
button { font: inherit; font-weight: 600; padding: 10px 20px; border-radius: 8px; border: 0; background: var(--accent); color: var(--bg); cursor: pointer; }
.warn { color: var(--warn); font-weight: 600; }
a { color: var(--accent); }
</style></head><body><main>
{{if .Problem}}
<h1>Storage keys</h1>
<p>{{.Problem}}</p>
{{else if .Show}}
<h1>Your storage account, {{.C.Name}}</h1>
<p class="warn">Save these now: this page can only be opened once.</p>
<div class="box"><dl>
<dt>Endpoint</dt><dd>{{.Endpoint}}</dd>
<dt>Access key</dt><dd>{{.C.AccessKey}}</dd>
<dt>Secret key</dt><dd>{{.C.Secret}}</dd>
<dt>Region</dt><dd>us-east-1</dd>
<dt>Space</dt><dd>{{.Space}}</dd>
{{if .TrialEnds}}<dt>Free trial until</dt><dd>{{.TrialEnds}}</dd>{{end}}
</dl></div>
<p>Keep the secret key private, like a password. Store it in your password manager.</p>
<p>Any program that supports <b>S3 compatible storage</b> works: rclone, Cyberduck, Duplicati and others.
Choose "S3 compatible" or "Other", use <b>path-style</b> addressing, and turn on the program's own encryption, so only you can read your files.</p>
<p>For <a href="https://rclone.org/downloads/" rel="noreferrer">rclone</a>, put this in its config file, then replace the password line with the output of <code>rclone obscure "a long passphrase"</code>:</p>
<pre>[group]
type = s3
provider = Other
access_key_id = {{.C.AccessKey}}
secret_access_key = {{.C.Secret}}
endpoint = {{.Endpoint}}
force_path_style = true

[safe]
type = crypt
remote = group:my-backup/encrypted
password = RUN: rclone obscure "a long passphrase"</pre>
<p class="sub">Full guide: <a href="https://github.com/peterretief/yggstore/blob/main/docs/gateway.md#for-customers" rel="noreferrer">setting up your program</a>.
{{if .TrialEnds}} When the trial ends you can still download your files for 30 days.{{end}}</p>
{{else}}
<h1>Your storage keys, {{.C.Name}}</h1>
<p>Press the button to see the keys for your storage account. <b>They are shown only once</b>, so have somewhere ready to save them, such as your password manager.</p>
<form method="post"><button type="submit">Show my keys</button></form>
<p class="sub" style="margin-top:16px">This link works until {{.Expires}}. If someone else may have opened it first, tell the organiser.</p>
{{end}}
</main></body></html>
`))

type settings struct {
	Endpoint string `json:"endpoint,omitempty"`
}

// Endpoint is the public address remembered from the last command that
// gave one, or "".
func Endpoint(dir string) string {
	var s settings
	readJSON(filepath.Join(dir, "settings.json"), &s)
	return s.Endpoint
}

// SetEndpoint remembers the gateway's public address.
func SetEndpoint(dir, endpoint string) error {
	var s settings
	readJSON(filepath.Join(dir, "settings.json"), &s)
	s.Endpoint = strings.TrimRight(endpoint, "/")
	return writeJSON(filepath.Join(dir, "settings.json"), s)
}
