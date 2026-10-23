package site

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	netmail "net/mail"
	"net/textproto"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/peterretief/yggstore/internal/mail"
	"github.com/peterretief/yggstore/internal/peers"
)

// Photos and files people add to a site (see docs/site-uploads.md). A page
// on the site posts one at a time to UploadPath, with an access token from
// the group's login; only the addresses the site's owner listed may. The
// web node seals it for the person the group gave the site's name to, as a
// contact form's message is, and it goes to their box like mail. The box
// checks the token again and puts the upload in the site.
const UploadPath = "/_yggstore/upload"

const (
	UploadMaxFile    = 10 << 20  // the most a file may be
	UploadMaxPreview = 256 << 10 // the most its preview may be
	uploadMaxCaption = 500       // characters
	uploadPerIP      = 30        // uploads per visitor address every uploadIPWindow
	uploadIPWindow   = 10 * time.Minute
	uploadPerSite    = 200 // uploads per site a day, per web node

	// UploadHeader marks a sealed message as an upload, naming its site.
	UploadHeader = "X-Yggstore-Upload"
	partHeader   = "X-Yggstore-Part"
)

// UploadTypes are the files a site takes, by their sniffed type, and the
// extension each is kept under. Nothing that runs in a browser.
var UploadTypes = map[string]string{
	"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp", "image/gif": ".gif", "application/pdf": ".pdf",
}

// Upload is one photo or file on its way to a site.
type Upload struct {
	Site    string
	Token   string // the uploader's access token, from the group's login
	Caption string
	Name    string // the file's name, made safe
	Type    string // one of UploadTypes
	File    []byte
	Preview []byte // a small JPEG (or PNG, WebP) the browser made; may be empty
}

func (w *Web) upload(rw http.ResponseWriter, r *http.Request, site string, uploaders []string) {
	rw.Header().Set("Cache-Control", "no-store")
	holder, pn, named := peers.NameHolder(w.Peers(), site)
	on := named && pn.Person != "" && pn.Code != "" && len(uploaders) > 0 && w.Who != nil && w.Deliver != nil
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	var who map[string]any
	signed := false
	if token = strings.TrimSpace(token); token != "" && w.Who != nil {
		who, signed = w.Who(r.Context(), token)
	}
	email, _ := who["email"].(string)
	email = strings.ToLower(email)
	may := on && signed && email != "" && slices.Contains(uploaders, email)
	switch {
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		// Whether to show the page's upload form.
		uploadReply(rw, http.StatusOK, map[string]any{"on": on, "signed_in": signed, "may": may})
		return
	case r.Method != http.MethodPost:
		uploadReply(rw, http.StatusMethodNotAllowed, map[string]any{"error": "Uploads are sent with POST."})
		return
	}
	if o := r.Header.Get("Origin"); o != "" && o != "null" {
		u, err := url.Parse(o)
		if err != nil || strings.TrimPrefix(Normalise(u.Hostname()), "www.") != strings.TrimPrefix(site, "www.") {
			uploadReply(rw, http.StatusForbidden, map[string]any{"error": "The upload must come from this site."})
			return
		}
	}
	switch {
	case !on:
		uploadReply(rw, http.StatusNotFound, map[string]any{"error": "This site doesn't take uploads."})
		return
	case !signed:
		uploadReply(rw, http.StatusUnauthorized, map[string]any{"error": "Sign in first."})
		return
	case !may:
		uploadReply(rw, http.StatusForbidden, map[string]any{"error": "You're not one of the people who may add things to this site: ask its owner."})
		return
	}
	ip := visitor(r)
	if !w.limit.allow("upload ip "+ip, uploadPerIP, uploadIPWindow) {
		uploadReply(rw, http.StatusTooManyRequests, map[string]any{"error": "Too many uploads from your address; please try again later."})
		return
	}
	if !w.limit.allow("upload site "+site, uploadPerSite, 24*time.Hour) {
		uploadReply(rw, http.StatusTooManyRequests, map[string]any{"error": "This site has had too many uploads today; please try again tomorrow."})
		return
	}
	up, err := readUploadForm(rw, r)
	if err != nil {
		uploadReply(rw, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	up.Site, up.Token = site, token
	name, _ := who["name"].(string)
	raw, err := UploadMessage(up, name, email, time.Now())
	if err == nil {
		raw, err = mail.Seal(pn.Code, raw)
	}
	if err != nil {
		w.logf("web: upload to %s: %v", site, err)
		uploadReply(rw, http.StatusInternalServerError, map[string]any{"error": "The upload could not be sent."})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	id, err := w.Deliver(ctx, holder.IP(), raw)
	if err != nil {
		w.logf("web: upload to %s: %v", site, err)
		uploadReply(rw, http.StatusServiceUnavailable, map[string]any{"error": "The upload could not be sent just now; please try again in a few minutes."})
		return
	}
	w.logf("web: upload to %s from %s: %s (%d bytes), message %s", site, email, up.Name, len(up.File), id)
	uploadReply(rw, http.StatusOK, map[string]any{"ok": true})
}

func uploadReply(rw http.ResponseWriter, code int, body map[string]any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(code)
	json.NewEncoder(rw).Encode(body)
}

// readUploadForm reads the page's form: "file", and optionally "preview"
// and "caption".
func readUploadForm(rw http.ResponseWriter, r *http.Request) (Upload, error) {
	var up Upload
	ct, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct != "multipart/form-data" {
		return up, errors.New("send the upload as a form with a file")
	}
	mr := multipart.NewReader(http.MaxBytesReader(rw, r.Body, UploadMaxFile+UploadMaxPreview+64<<10), params["boundary"])
	fileName := ""
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return up, errors.New("the upload could not be read (or is too large)")
		}
		switch p.FormName() {
		case "file":
			if up.File, err = readAtMost(p, UploadMaxFile, "the file is larger than 10 MB"); err != nil {
				return up, err
			}
			fileName = p.FileName()
		case "preview":
			if up.Preview, err = readAtMost(p, UploadMaxPreview, "the preview is too large"); err != nil {
				return up, err
			}
		case "caption":
			b, err := readAtMost(p, 4*uploadMaxCaption, "the caption is too long")
			if err != nil {
				return up, err
			}
			up.Caption = string(b)
		}
	}
	return up, up.check(fileName)
}

func readAtMost(r io.Reader, max int64, tooBig string) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, errors.New("the upload could not be read")
	}
	if int64(len(b)) > max {
		return nil, errors.New(tooBig)
	}
	return b, nil
}

// check makes the upload safe to keep: a type the site takes, whatever the
// name says; a short caption; a safe file name.
func (up *Upload) check(fileName string) error {
	if len(up.File) == 0 {
		return errors.New("there's no file in the upload")
	}
	up.Type = strings.Split(http.DetectContentType(up.File), ";")[0]
	ext, ok := UploadTypes[up.Type]
	if !ok {
		return errors.New("this site takes photos (JPEG, PNG, WebP, GIF) and PDFs only")
	}
	if len(up.Preview) > 0 {
		pt := strings.Split(http.DetectContentType(up.Preview), ";")[0]
		if pt != "image/jpeg" && pt != "image/png" && pt != "image/webp" {
			up.Preview = nil // not a preview after all: show an icon
		}
	}
	up.Caption = oneLine(up.Caption, uploadMaxCaption)
	up.Name = SafeName(fileName, ext)
	return nil
}

// SafeName is a file name fit to keep on a site: no folders, nothing odd,
// not too long, and ending in ext.
func SafeName(name, ext string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.TrimSuffix(name, path.Ext(name))
	name = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.':
			return r
		case unicode.IsSpace(r):
			return '-'
		}
		return -1
	}, name)
	name = strings.Trim(name, ".-")
	if r := []rune(name); len(r) > 80 {
		name = string(r[:80])
	}
	if name == "" {
		name = "file"
	}
	return name + ext
}

// UploadMessage writes an upload up as a message for the site's owner:
// headers that say what it is, then its caption, token, file and preview
// as parts.
func UploadMessage(up Upload, name, email string, now time.Time) ([]byte, error) {
	idb := make([]byte, 12)
	rand.Read(idb)
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	hdr := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	from := (&netmail.Address{Name: name, Address: email}).String()
	what := "A photo"
	if !strings.HasPrefix(up.Type, "image/") {
		what = "A file"
	}
	hdr("From", from)
	hdr("To", (&netmail.Address{Name: "Uploads to " + up.Site, Address: "uploads@" + up.Site}).String())
	hdr("Subject", mime.QEncoding.Encode("utf-8", "["+up.Site+"] "+what+" from "+oneLine(name, 60)+": "+up.Name))
	hdr("Date", now.Format(time.RFC1123Z))
	hdr("Message-ID", "<"+hex.EncodeToString(idb)+"@"+up.Site+">")
	hdr(UploadHeader, up.Site)
	hdr("MIME-Version", "1.0")
	hdr("Content-Type", "multipart/mixed; boundary="+mw.Boundary())
	b.WriteString("\r\n")
	part := func(kind, ctype, disposition string, data []byte) error {
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", ctype)
		h.Set("Content-Transfer-Encoding", "base64")
		h.Set(partHeader, kind)
		if disposition != "" {
			h.Set("Content-Disposition", disposition)
		}
		pw, err := mw.CreatePart(h)
		if err != nil {
			return err
		}
		enc := base64.StdEncoding.EncodeToString(data)
		for len(enc) > 76 {
			io.WriteString(pw, enc[:76]+"\r\n")
			enc = enc[76:]
		}
		_, err = io.WriteString(pw, enc+"\r\n")
		return err
	}
	file := mime.FormatMediaType("attachment", map[string]string{"filename": up.Name})
	err := errors.Join(
		part("caption", "text/plain; charset=utf-8", "", []byte(up.Caption)),
		part("token", "application/jwt", "", []byte(up.Token)),
		part("file", up.Type, file, up.File))
	if err == nil && len(up.Preview) > 0 {
		err = part("preview", strings.Split(http.DetectContentType(up.Preview), ";")[0], `inline; filename="preview"`, up.Preview)
	}
	if err == nil {
		err = mw.Close()
	}
	return b.Bytes(), err
}

// ReadUpload reads a message UploadMessage wrote; ok is false if raw isn't
// an upload. It checks the file again, as a message can come from anyone
// who has the owner's sharing code: who sent it is up to the token.
func ReadUpload(raw []byte) (up Upload, ok bool, err error) {
	m, err := netmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return up, false, nil
	}
	up.Site = Normalise(m.Header.Get(UploadHeader))
	if up.Site == "" {
		return up, false, nil
	}
	ct, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil || ct != "multipart/mixed" {
		return up, true, errors.New("the upload has no parts")
	}
	mr := multipart.NewReader(m.Body, params["boundary"])
	fileName := ""
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return up, true, err
		}
		data, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, io.LimitReader(p, 2*UploadMaxFile)))
		if err != nil {
			return up, true, fmt.Errorf("reading its %s: %w", p.Header.Get(partHeader), err)
		}
		switch p.Header.Get(partHeader) {
		case "caption":
			up.Caption = string(data)
		case "token":
			up.Token = strings.TrimSpace(string(data))
		case "file":
			if len(data) > UploadMaxFile {
				return up, true, errors.New("the file is too large")
			}
			up.File = data
			_, dp, _ := mime.ParseMediaType(p.Header.Get("Content-Disposition"))
			fileName = dp["filename"]
		case "preview":
			if len(data) <= UploadMaxPreview {
				up.Preview = data
			}
		}
	}
	return up, true, up.check(fileName)
}
