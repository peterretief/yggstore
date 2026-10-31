package site

import (
	"crypto/subtle"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// A site's preview (see PreviewPath): the preview link sets a cookie, and
// a browser with it sees the preview version on the site's own address,
// with a bar saying so. Only the key's hash is on the web nodes.

const previewCookie = "yggstore_preview"

func inPreview(r *http.Request, keyHash string) bool {
	c, err := r.Cookie(previewCookie)
	return err == nil && subtle.ConstantTimeCompare([]byte(PreviewKeyHash(c.Value)), []byte(keyHash)) == 1
}

// previewSwitch opens the preview (?key=KEY) or leaves it (?leave=1).
func previewSwitch(rw http.ResponseWriter, r *http.Request, keyHash string) {
	rw.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	if q.Get("leave") != "" {
		http.SetCookie(rw, &http.Cookie{Name: previewCookie, Value: "", Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
		http.Redirect(rw, r, "/", http.StatusSeeOther)
		return
	}
	key := q.Get("key")
	if keyHash == "" || key == "" || subtle.ConstantTimeCompare([]byte(PreviewKeyHash(key)), []byte(keyHash)) != 1 {
		http.Error(rw, "This preview link isn't open any more: the preview went live, or was dropped.", http.StatusNotFound)
		return
	}
	http.SetCookie(rw, &http.Cookie{Name: previewCookie, Value: key, Path: "/", MaxAge: 7 * 86400, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	http.Redirect(rw, r, "/", http.StatusSeeOther)
}

// previewBar goes at the end of every page of a preview.
const previewBar = `<div style="position:fixed;left:0;right:0;bottom:0;z-index:2147483647;background:#b7791f;color:#fff;` +
	`font:14px/1.4 system-ui,sans-serif;padding:8px 12px;text-align:center">` +
	`Preview: not live yet. <a href="` + PreviewPath + `?leave=1" style="color:#fff;font-weight:600">Leave preview</a></div>`

// servePreview serves the preview's folder, with the bar on its pages.
func servePreview(rw http.ResponseWriter, r *http.Request, root string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		serveStatic(rw, r, root)
		return
	}
	rel := filepath.FromSlash(filepath.Clean("/" + r.URL.Path))
	file := filepath.Join(root, rel)
	if fi, err := os.Stat(file); err == nil && fi.IsDir() {
		if !strings.HasSuffix(r.URL.Path, "/") {
			serveStatic(rw, r, root) // the file server adds the slash
			return
		}
		file = filepath.Join(file, "index.html")
	}
	ext := strings.ToLower(filepath.Ext(file))
	b, err := os.ReadFile(file)
	if err != nil || (ext != ".html" && ext != ".htm") {
		serveStatic(rw, r, root)
		return
	}
	page := string(b)
	if i := strings.LastIndex(strings.ToLower(page), "</body>"); i >= 0 {
		page = page[:i] + previewBar + page[i:]
	} else {
		page += previewBar
	}
	rw.Header().Set("Content-Type", "text/html")
	rw.Header().Set("X-Content-Type-Options", "nosniff")
	rw.Write([]byte(page))
}
