package mail

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	netmail "net/mail"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/atomicfile"
	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/share"
)

// Box reads a mailbox that a Node collects into.
type Box struct {
	Dir string
	ID  *share.Identity

	mu    sync.Mutex
	cache map[string]Summary // by ID; messages don't change
}

// Summary is one message in a list.
type Summary struct {
	ID       string `json:"id"`
	From     string `json:"from"`
	To       string `json:"to"`
	Subject  string `json:"subject"`
	Date     int64  `json:"date"`           // unix ms, from the Date header
	Received int64  `json:"received"`       // unix ms, when the group took it (or it was sent)
	Sent     bool   `json:"sent,omitempty"` // written here, not received
	Size     int    `json:"size"`
	Error    string `json:"error,omitempty"`
}

// Message is one message, read.
type Message struct {
	Summary
	Cc          string       `json:"cc,omitempty"`
	ReplyTo     string       `json:"reply_to,omitempty"`
	Text        string       `json:"text"`
	FromHTML    bool         `json:"from_html,omitempty"` // Text was made from the HTML part
	MessageID   string       `json:"message_id,omitempty"`
	References  string       `json:"references,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

// Attachment is a part of a message with a file name.
type Attachment struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Size int    `json:"size"`
	data []byte
}

// ErrNoMessage means the mailbox has no message with that ID.
var ErrNoMessage = errors.New("no such message")

// List returns the mailbox, newest first.
func (b *Box) List() ([]Summary, error) {
	ents, err := os.ReadDir(b.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return []Summary{}, nil
	} else if err != nil {
		return nil, err
	}
	out := []Summary{}
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), ".sealed")
		if !ok || !validID.MatchString(id) {
			continue
		}
		b.mu.Lock()
		s, ok := b.cache[id]
		b.mu.Unlock()
		if !ok {
			m, err := b.Read(id)
			if err != nil {
				s = Summary{ID: id, Error: err.Error()}
			} else {
				s = m.Summary
				b.mu.Lock()
				if b.cache == nil {
					b.cache = map[string]Summary{}
				}
				b.cache[id] = s
				b.mu.Unlock()
			}
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Received > out[j].Received })
	return out, nil
}

// Raw returns a message as it arrived (an .eml file).
func (b *Box) Raw(id string) ([]byte, error) {
	if !validID.MatchString(id) {
		return nil, ErrNoMessage
	}
	sealed, err := os.ReadFile(filepath.Join(b.Dir, id+".sealed"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoMessage
	} else if err != nil {
		return nil, err
	}
	if b.ID == nil {
		return nil, errors.New("no sharing key to open mail with")
	}
	return Open(b.ID, sealed)
}

// Read opens and parses a message.
func (b *Box) Read(id string) (Message, error) {
	raw, err := b.Raw(id)
	if err != nil {
		return Message{}, err
	}
	m, err := Parse(raw)
	m.ID = id
	var meta Meta
	if data, err := os.ReadFile(filepath.Join(b.Dir, id+".json")); err == nil {
		json.Unmarshal(data, &meta)
	}
	m.Received, m.Sent = meta.Received, meta.Sent
	return m, err
}

// Attachment returns the nth attachment of a message.
func (b *Box) Attachment(id string, n int) (Attachment, []byte, error) {
	raw, err := b.Raw(id)
	if err != nil {
		return Attachment{}, nil, err
	}
	m, err := Parse(raw)
	if err != nil {
		return Attachment{}, nil, err
	}
	if n < 0 || n >= len(m.Attachments) {
		return Attachment{}, nil, ErrNoMessage
	}
	a := m.Attachments[n]
	return a, a.data, nil
}

// Delete removes a message from the mailbox and its copy from the group.
func (b *Box) Delete(ctx context.Context, c client.Client, id string) error {
	if !validID.MatchString(id) {
		return ErrNoMessage
	}
	stub := filepath.Join(b.Dir, id+".ystub")
	if _, err := os.Stat(stub); err == nil {
		if _, res, err := files.DeleteStub(ctx, c, stub); err != nil {
			return err
		} else if res.Err() != nil {
			return res.Err()
		}
	}
	for _, ext := range []string{".sealed", ".json", ".ystub"} {
		if err := os.Remove(filepath.Join(b.Dir, id+ext)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	b.mu.Lock()
	delete(b.cache, id)
	b.mu.Unlock()
	return nil
}

var words = &mime.WordDecoder{CharsetReader: charsetReader}

func header(h netmail.Header, k string) string {
	v := h.Get(k)
	if d, err := words.DecodeHeader(v); err == nil {
		v = d
	}
	return strings.Join(strings.Fields(v), " ")
}

// Parse reads a message: its headers, its text, and its attachments.
func Parse(raw []byte) (Message, error) {
	var m Message
	m.Size = len(raw)
	msg, err := netmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		m.Subject = "(unreadable message)"
		m.Text = string(raw)
		return m, nil
	}
	m.From = header(msg.Header, "From")
	m.To = header(msg.Header, "To")
	m.Cc = header(msg.Header, "Cc")
	m.ReplyTo = header(msg.Header, "Reply-To")
	m.Subject = header(msg.Header, "Subject")
	m.MessageID = strings.TrimSpace(msg.Header.Get("Message-ID"))
	m.References = strings.Join(strings.Fields(msg.Header.Get("References")), " ")
	if t, err := msg.Header.Date(); err == nil {
		m.Date = t.UnixMilli()
	}
	var plain, htmlText string
	walk(msg.Header, msg.Body, 0, func(ctype string, params map[string]string, disp string, name string, body []byte) {
		switch {
		case name != "" || disp == "attachment":
			if name == "" {
				name = "attachment"
			}
			m.Attachments = append(m.Attachments, Attachment{Name: name, Type: ctype, Size: len(body), data: body})
		case ctype == "text/plain" && plain == "":
			plain = decodeCharset(body, params["charset"])
		case ctype == "text/html" && htmlText == "":
			htmlText = decodeCharset(body, params["charset"])
		}
	})
	switch {
	case strings.TrimSpace(plain) != "":
		m.Text = plain
	case htmlText != "":
		m.Text, m.FromHTML = HTMLToText(htmlText), true
	}
	return m, nil
}

type partHeader interface{ Get(string) string }

// walk calls f for every leaf part of a message, decoded.
func walk(h partHeader, body io.Reader, depth int, f func(ctype string, params map[string]string, disp, name string, body []byte)) {
	ctype, params, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil {
		ctype, params = "text/plain", map[string]string{}
	}
	if strings.HasPrefix(ctype, "multipart/") && depth < 10 {
		mr := multipart.NewReader(body, params["boundary"])
		for {
			p, err := mr.NextRawPart()
			if err != nil {
				return
			}
			walk(p.Header, p, depth+1, f)
		}
	}
	data, _ := io.ReadAll(io.LimitReader(body, MaxSize))
	switch strings.ToLower(strings.TrimSpace(h.Get("Content-Transfer-Encoding"))) {
	case "base64":
		clean := strings.TrimRight(strings.Join(strings.Fields(string(data)), ""), "=")
		if d, err := base64.RawStdEncoding.DecodeString(clean); err == nil {
			data = d
		}
	case "quoted-printable":
		if d, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(data))); err == nil {
			data = d
		}
	}
	disp, dparams, _ := mime.ParseMediaType(h.Get("Content-Disposition"))
	name := dparams["filename"]
	if name == "" {
		name = params["name"]
	}
	if d, err := words.DecodeHeader(name); err == nil {
		name = d
	}
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	if name == "." || name == "/" {
		name = ""
	}
	if ctype == "message/rfc822" && name == "" {
		name = "message.eml"
	}
	f(ctype, params, disp, name, data)
}

func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	data, err := io.ReadAll(input)
	if err != nil {
		return nil, err
	}
	return strings.NewReader(decodeCharset(data, charset)), nil
}

// decodeCharset makes text UTF-8. Besides UTF-8 it knows Latin-1 and
// Windows-1252, which cover most other mail; anything else is shown as is.
func decodeCharset(data []byte, charset string) string {
	switch strings.ToLower(charset) {
	case "iso-8859-1", "latin1":
		return latin(data, nil)
	case "windows-1252", "cp1252":
		return latin(data, cp1252)
	}
	return strings.ToValidUTF8(string(data), "\uFFFD")
}

func latin(data []byte, extra map[byte]rune) string {
	var sb strings.Builder
	for _, c := range data {
		if r, ok := extra[c]; ok {
			sb.WriteRune(r)
		} else {
			sb.WriteRune(rune(c))
		}
	}
	return sb.String()
}

var cp1252 = map[byte]rune{0x80: '€', 0x82: '‚', 0x84: '„', 0x85: '…', 0x91: '‘', 0x92: '’', 0x93: '“', 0x94: '”', 0x95: '•', 0x96: '–', 0x97: '—', 0x99: '™'}

var (
	dropBlocks = regexp.MustCompile(`(?is)<(script|style|head|title)\b.*?</(script|style|head|title)\s*>`)
	breaks     = regexp.MustCompile(`(?i)<(br|/p|/div|/tr|/h[1-6]|/li|/blockquote|/table)\b[^>]*>`)
	items      = regexp.MustCompile(`(?i)<li\b[^>]*>`)
	links      = regexp.MustCompile(`(?is)<a\b[^>]*\bhref\s*=\s*["']?([^"' >]+)["']?[^>]*>(.*?)</a\s*>`)
	tags       = regexp.MustCompile(`(?s)<[^>]*>`)
	blankLines = regexp.MustCompile(`\n[ \t]*(\n[ \t]*)+\n`)
)

// HTMLToText turns an HTML message into plain text, keeping link targets.
func HTMLToText(s string) string {
	s = dropBlocks.ReplaceAllString(s, "")
	s = links.ReplaceAllStringFunc(s, func(a string) string {
		m := links.FindStringSubmatch(a)
		text := strings.TrimSpace(tags.ReplaceAllString(m[2], ""))
		href := html.UnescapeString(m[1])
		if text == "" || text == href || !strings.HasPrefix(href, "http") {
			return text + " " + href
		}
		return text + " (" + href + ")"
	})
	s = items.ReplaceAllString(s, "\n• ")
	s = breaks.ReplaceAllString(s, "\n")
	s = tags.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		lines = append(lines, strings.Join(strings.Fields(l), " "))
	}
	s = strings.Join(lines, "\n")
	s = blankLines.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// addressFile, in the mailbox, is the owner's own address, for sending.
const addressFile = "address"

// Address is the mailbox owner's address ("" if not set).
func (b *Box) Address() string {
	data, _ := os.ReadFile(filepath.Join(b.Dir, addressFile))
	return strings.TrimSpace(string(data))
}

// SetAddress keeps the owner's address.
func (b *Box) SetAddress(addr string) error {
	if err := os.MkdirAll(b.Dir, 0o700); err != nil {
		return err
	}
	return atomicfile.Replace(filepath.Join(b.Dir, addressFile), []byte(addr+"\n"), 0o600)
}

// KeepSent puts a sent message in the mailbox, sealed for the owner, and
// stores a copy in the group. The message is kept even if the group copy
// fails; that error is returned with its ID.
func (b *Box) KeepSent(ctx context.Context, c client.Client, list []peers.Peer, raw []byte) (string, error) {
	if b.ID == nil {
		return "", errors.New("no sharing key to keep mail with")
	}
	sealed, err := Seal(b.ID.Code(), raw)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(b.Dir, 0o700); err != nil {
		return "", err
	}
	id := "sent-" + randomID()
	meta, _ := json.Marshal(Meta{Received: time.Now().UnixMilli(), Sent: true})
	if err := atomicfile.Replace(filepath.Join(b.Dir, id+".json"), meta, 0o600); err != nil {
		return "", err
	}
	if err := atomicfile.Replace(filepath.Join(b.Dir, id+".sealed"), sealed, 0o600); err != nil {
		return "", err
	}
	m, _, _, err := files.PutReader(ctx, c, bytes.NewReader(sealed), "mail-"+id, online(ctx, c, list), files.PutOptions{})
	if err != nil {
		return id, fmt.Errorf("kept here, but not stored in the group: %w", err)
	}
	return id, files.WriteJSON(filepath.Join(b.Dir, id+".ystub"), m)
}
