package hub

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/atomicfile"
	"github.com/peterretief/yggstore/internal/light"
	"github.com/peterretief/yggstore/internal/outbox"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/site"
)

// Photos and files added to people's sites (see docs/site-uploads.md). A
// web node sends each one to the site's owner like mail, sealed for them.
// Anyone with their sharing code can send them mail, so the box believes
// an upload only if it carries an access token the group's login made for
// this site, for someone on the site's list. Then it goes in the site's
// uploads folder, which the site's own pages show.

// UploadsDir is the folder in a site that uploads go in.
const UploadsDir = "uploads"

// uploadTokenAge is how old an upload's token may be when the box files
// it: the box may have been off while it waited.
const uploadTokenAge = 30 * 24 * time.Hour

// Uploaded is an upload as the site shows it (uploads/ID.json, and
// uploads/index.json lists them all, newest first). It names who added it,
// not their address, since the site is public.
type Uploaded struct {
	ID      string `json:"id"`
	File    string `json:"file"`              // in uploads/
	Preview string `json:"preview,omitempty"` // in uploads/
	Name    string `json:"name"`              // the file's name as it was added
	Type    string `json:"type"`
	Size    int    `json:"size"`
	Caption string `json:"caption,omitempty"`
	By      string `json:"by"`
	At      int64  `json:"at"` // unix seconds
}

// fileUpload puts raw, a message for p, in their site if it's an upload
// that checks out. False if it isn't one, or doesn't: then it's mail.
func (ps *PeopleSites) fileUpload(ctx context.Context, p Person, raw []byte, list []peers.Peer) bool {
	up, ok, err := site.ReadUpload(raw)
	if !ok {
		return false
	}
	var who map[string]any
	if err == nil {
		who, err = ps.checkUpload(ctx, p, up, list)
	}
	var u Uploaded
	if err == nil {
		u, err = ps.keepUpload(p, up, who)
	}
	if err != nil {
		ps.cfg.Logf("%s: an upload to %s isn't going on the site (it goes in their messages): %v", p.Name, up.Site, err)
		return false
	}
	ps.cfg.Logf("%s: %s added %s to %s", p.Name, u.By, u.Name, up.Site)
	return true
}

// checkUpload says whether up may go on p's site: one of their names, with
// a token the group's login made for that site, for someone on its list.
// It gives who that is (the token's claims).
func (ps *PeopleSites) checkUpload(ctx context.Context, p Person, up site.Upload, list []peers.Peer) (map[string]any, error) {
	r := ps.site(p)
	if r == nil || !slices.Contains(r.names, up.Site) {
		return nil, fmt.Errorf("%s isn't their site", up.Site)
	}
	c, ok := light.VerifyToken(up.Token, ps.loginKeys(ctx, false))
	if !ok {
		c, ok = light.VerifyToken(up.Token, ps.loginKeys(ctx, true)) // a web node's new key, say
	}
	if !ok {
		return nil, errors.New("its token isn't one the group's login made")
	}
	exp, _ := c["exp"].(float64)
	email, _ := c["email"].(string)
	switch {
	case c["use"] != "access" || c["aud"] != up.Site:
		return nil, errors.New("its token isn't for uploading to this site")
	case c["iss"] != "https://"+site.LightPrefix+peers.GroupDomain(list):
		return nil, errors.New("its token is from another login")
	case time.Unix(int64(exp), 0).Before(time.Now().Add(-uploadTokenAge)):
		return nil, errors.New("its token is too old")
	case !slices.Contains(r.pub.Uploaders(up.Site), strings.ToLower(email)):
		return nil, fmt.Errorf("%s isn't on the site's list", email)
	}
	return c, nil
}

// keepUpload writes up into the site's uploads folder, and remembers on the
// box (not on the site) whose address it came from.
func (ps *PeopleSites) keepUpload(p Person, up site.Upload, who map[string]any) (Uploaded, error) {
	dir := filepath.Join(p.Path, outbox.WebsitesDir, up.Site, UploadsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Uploaded{}, err
	}
	rb := make([]byte, 3)
	rand.Read(rb)
	now := time.Now()
	id := now.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(rb)
	file := up.File
	if up.Type == "image/jpeg" {
		var err error
		if file, err = jpegWithoutMetadata(file); err != nil {
			return Uploaded{}, fmt.Errorf("the photo couldn't be read: %w", err)
		}
	}
	by, _ := who["name"].(string)
	email, _ := who["email"].(string)
	u := Uploaded{ID: id, File: id + filepath.Ext(up.Name), Name: up.Name, Type: up.Type, Size: len(file), Caption: up.Caption, By: by, At: now.Unix()}
	if len(up.Preview) > 0 {
		u.Preview = id + ".preview" + map[bool]string{true: ".png", false: ".jpg"}[bytes.HasPrefix(up.Preview, []byte("\x89PNG"))]
		if err := atomicfile.Replace(filepath.Join(dir, u.Preview), up.Preview, 0o644); err != nil {
			return Uploaded{}, err
		}
	}
	if err := atomicfile.Replace(filepath.Join(dir, u.File), file, 0o644); err != nil {
		return Uploaded{}, err
	}
	b, _ := json.MarshalIndent(u, "", "  ")
	if err := atomicfile.Replace(filepath.Join(dir, id+".json"), b, 0o644); err != nil {
		return Uploaded{}, err
	}
	logLine, _ := json.Marshal(map[string]any{"id": id, "site": up.Site, "by": by, "email": email, "at": now.Unix(), "name": up.Name})
	if f, err := os.OpenFile(filepath.Join(ps.cfg.Dir, p.Folder, "uploads.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		f.Write(append(logLine, '\n'))
		f.Close()
	}
	_, err := indexUploads(dir)
	return u, err
}

// loginKeys are the keys the group's login signs with, asked of the web
// nodes now and then, or now if fresh.
func (ps *PeopleSites) loginKeys(ctx context.Context, fresh bool) []light.JWK {
	ps.mu.Lock()
	keys, at := ps.keys, ps.keysAt
	ps.mu.Unlock()
	if ps.cfg.LoginKeys == nil || (len(keys) > 0 && time.Since(at) < 10*time.Minute && !(fresh && time.Since(at) > time.Minute)) {
		return keys
	}
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if got := ps.cfg.LoginKeys(cctx); len(got) > 0 {
		keys = got
	}
	ps.mu.Lock()
	ps.keys, ps.keysAt = keys, time.Now()
	ps.mu.Unlock()
	return keys
}

// indexUploads writes dir's index.json from its ID.json files, newest
// first. An upload whose file was deleted is left out, and its preview and
// ID.json go too. It says whether the index changed.
func indexUploads(dir string) (bool, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	list := []Uploaded{}
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || id == "index" || e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		var u Uploaded
		if err != nil || json.Unmarshal(b, &u) != nil || u.ID != id || filepath.Base(u.File) != u.File || u.File == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, u.File)); errors.Is(err, os.ErrNotExist) {
			if u.Preview != "" && filepath.Base(u.Preview) == u.Preview {
				os.Remove(filepath.Join(dir, u.Preview))
			}
			os.Remove(filepath.Join(dir, e.Name()))
			continue
		}
		list = append(list, u)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID > list[j].ID })
	b, _ := json.MarshalIndent(map[string]any{"uploads": list}, "", "  ")
	at := filepath.Join(dir, "index.json")
	if old, err := os.ReadFile(at); err == nil && bytes.Equal(old, b) {
		return false, nil
	}
	return true, atomicfile.Replace(at, b, 0o644)
}

// indexAll brings every running site's upload list up to date, for
// uploads deleted on the files page.
func (ps *PeopleSites) indexAll(st HubState) {
	for _, p := range st.People {
		r := ps.site(p)
		if r == nil {
			continue
		}
		for _, name := range r.names {
			dir := filepath.Join(p.Path, outbox.WebsitesDir, name, UploadsDir)
			if _, err := os.Stat(dir); err == nil {
				if _, err := indexUploads(dir); err != nil {
					ps.logOnce(p.Name, "the list of uploads on %s: %v", name, err)
				}
			}
		}
	}
}

// jpegWithoutMetadata drops what a JPEG says besides the picture: its EXIF
// (where it was taken, the camera, when), XMP and IPTC blocks. It keeps
// which way up it goes, in a small EXIF block of its own, as phones turn
// photos that way.
func jpegWithoutMetadata(b []byte) ([]byte, error) {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return nil, errors.New("not a JPEG")
	}
	var kept [][]byte
	orient := 0
	i := 2
	for {
		if i+4 > len(b) || b[i] != 0xFF {
			return nil, errors.New("a damaged JPEG")
		}
		m := b[i+1]
		if m == 0xFF { // fill
			i++
			continue
		}
		if m == 0xDA { // start of scan: the picture itself, to the end
			kept = append(kept, b[i:])
			break
		}
		n := int(b[i+2])<<8 | int(b[i+3])
		if n < 2 || i+2+n > len(b) {
			return nil, errors.New("a damaged JPEG")
		}
		seg := b[i : i+2+n]
		switch m {
		case 0xE1: // EXIF or XMP
			if orient == 0 {
				orient = exifOrientation(seg[4:])
			}
		case 0xED: // IPTC
		default:
			kept = append(kept, seg)
		}
		i += 2 + n
	}
	out := []byte{0xFF, 0xD8}
	if len(kept) > 0 && len(kept[0]) > 1 && kept[0][1] == 0xE0 { // JFIF comes first
		out, kept = append(out, kept[0]...), kept[1:]
	}
	if orient > 1 && orient <= 8 {
		out = append(out, 0xFF, 0xE1, 0x00, 0x22)
		out = append(out, "Exif\x00\x00MM\x00\x2a\x00\x00\x00\x08\x00\x01\x01\x12\x00\x03\x00\x00\x00\x01"...)
		out = append(out, 0x00, byte(orient), 0x00, 0x00, 0x00, 0x00, 0x00, 0x00)
	}
	for _, k := range kept {
		out = append(out, k...)
	}
	return out, nil
}

// exifOrientation reads which way up a photo goes from an EXIF block
// ("Exif\0\0" and a TIFF header): 1 to 8, or 0 if it doesn't say.
func exifOrientation(d []byte) int {
	t, ok := bytes.CutPrefix(d, []byte("Exif\x00\x00"))
	if !ok || len(t) < 8 {
		return 0
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 0
	}
	ifd := int(bo.Uint32(t[4:8]))
	if ifd < 8 || ifd+2 > len(t) {
		return 0
	}
	n := int(bo.Uint16(t[ifd:]))
	for k := range n {
		e := ifd + 2 + 12*k
		if e+12 > len(t) {
			return 0
		}
		if bo.Uint16(t[e:]) == 0x0112 && bo.Uint16(t[e+2:]) == 3 {
			return int(bo.Uint16(t[e+8:]))
		}
	}
	return 0
}
