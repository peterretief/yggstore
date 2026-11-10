package app

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Machines share their logs by asking each other for what they haven't got.
// Each request goes through the group's node on the other machine, which
// passes it to the program (site.LocalRelay), so no new port is needed.
// Nodes see only sealed bytes: asking and answering both need the key that
// only your machines hold, which is copied to a new machine by hand.

const (
	syncPath    = base + "_sync"
	syncEvery   = 30 * time.Second // by default; see -sync-every
	syncMaxSend = 5000 // entries in one answer
	syncMaxAge  = 5 * time.Minute
)

// keeper is what holds the key and how the machines went.
type keeper struct {
	key  []byte
	mu   sync.Mutex
	seen map[string]string // machine address to how it last went
}

func newSyncKey(path string) error {
	b := make([]byte, 32)
	rand.Read(b)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(hex.EncodeToString(b) + "\n")
	return err
}

func readSyncKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(key) != 32 {
		return nil, errors.New(path + " isn't a sync key")
	}
	return key, nil
}

func aead(key []byte) (cipher.AEAD, error) {
	c, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(c)
}

// seal encrypts for one purpose ("ask" or "answer"), so neither can be
// passed off as the other.
func seal(key []byte, purpose string, plain []byte) ([]byte, error) {
	g, err := aead(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, g.NonceSize())
	rand.Read(nonce)
	return g.Seal(nonce, nonce, plain, []byte(purpose)), nil
}

func unseal(key []byte, purpose string, b []byte) ([]byte, error) {
	g, err := aead(key)
	if err != nil {
		return nil, err
	}
	if len(b) < g.NonceSize() {
		return nil, errors.New("too short")
	}
	return g.Open(nil, b[:g.NonceSize()], b[g.NonceSize():], []byte(purpose))
}

type syncAsk struct {
	At   int64            `json:"at"`
	ID   string           `json:"id"`
	Have map[string]int64 `json:"have"` // machine to its last entry here
}

type syncAnswer struct {
	ID      string  `json:"id"`
	Entries []entry `json:"entries"`
	More    bool    `json:"more"`
}

func (l *syncLog) have() (map[string]int64, error) {
	rows, err := l.d.db.Query(`SELECT node, MAX(seq) FROM _sync_entries GROUP BY node`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var n string
		var s int64
		if err := rows.Scan(&n, &s); err != nil {
			return nil, err
		}
		out[n] = s
	}
	return out, rows.Err()
}

// missing is what another machine hasn't got. A machine's entries always
// go in order, so what it has can be said with one number.
func (l *syncLog) missing(have map[string]int64) (out []entry, more bool, err error) {
	mine, err := l.have()
	if err != nil {
		return nil, false, err
	}
	for node, last := range mine {
		if last <= have[node] {
			continue
		}
		if len(out) >= syncMaxSend {
			return out, true, nil
		}
		list, err := readEntries(l.d.db, "WHERE node = ? AND seq > ?", node, have[node])
		if err != nil {
			return nil, false, err
		}
		if room := syncMaxSend - len(out); len(list) > room {
			list, more = list[:room], true
		}
		out = append(out, list...)
	}
	return out, more, nil
}

// serveSync answers another machine.
func (d *data) serveSync(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "too big", http.StatusRequestEntityTooLarge)
		return
	}
	plain, err := unseal(d.keep.key, "ask", body)
	var ask syncAsk
	if err == nil {
		err = json.Unmarshal(plain, &ask)
	}
	if err != nil || time.Since(time.UnixMilli(ask.At)).Abs() > syncMaxAge {
		http.Error(w, "no", http.StatusForbidden)
		return
	}
	entries, more, err := d.sync.missing(ask.Have)
	if err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	b, _ := json.Marshal(syncAnswer{ID: ask.ID, Entries: entries, More: more})
	sealed, err := seal(d.keep.key, "answer", b)
	if err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	w.Write(sealed)
}

// pull takes what a machine has that this one hasn't.
func (d *data) pull(client *http.Client, url string) (added int, err error) {
	for {
		have, err := d.sync.have()
		if err != nil {
			return added, err
		}
		id := make([]byte, 16)
		rand.Read(id)
		ask := syncAsk{At: time.Now().UnixMilli(), ID: hex.EncodeToString(id), Have: have}
		b, _ := json.Marshal(ask)
		sealed, err := seal(d.keep.key, "ask", b)
		if err != nil {
			return added, err
		}
		res, err := client.Post(url, "application/octet-stream", bytes.NewReader(sealed))
		if err != nil {
			return added, err
		}
		body, err := io.ReadAll(io.LimitReader(res.Body, 256<<20))
		res.Body.Close()
		if err != nil {
			return added, err
		}
		if res.StatusCode != http.StatusOK {
			return added, fmt.Errorf("it answered %s", res.Status)
		}
		plain, err := unseal(d.keep.key, "answer", body)
		if err != nil {
			return added, errors.New("its answer doesn't open: does it have the same key?")
		}
		var a syncAnswer
		if err := json.Unmarshal(plain, &a); err != nil {
			return added, err
		}
		if a.ID != ask.ID {
			return added, errors.New("the answer is to something else")
		}
		n, err := d.sync.merge(a.Entries)
		added += n
		if err != nil || !a.More {
			return added, err
		}
	}
}

// syncWith keeps asking the other machines, every so often, for as long as
// the program runs. An address is another machine's node ([ADDRESS]:7400),
// which passes the request to the program there; or, for two copies on one
// machine in development, the other program itself (http://127.0.0.1:PORT).
func (d *data) syncWith(site string, addrs []string, every time.Duration) {
	client := &http.Client{Timeout: 60 * time.Second}
	for {
		for _, addr := range addrs {
			url := "http://" + addr + "/v1/local/" + site + syncPath
			if strings.HasPrefix(addr, "http://") {
				url = strings.TrimSuffix(addr, "/") + syncPath
			}
			n, err := d.pull(client, url)
			d.keep.mu.Lock()
			was := d.keep.seen[addr]
			now := "in step at " + time.Now().Format("15:04")
			if err != nil {
				now = "not reached since " + strings.TrimPrefix(strings.TrimPrefix(was, "not reached since "), "in step at ")
				if !strings.HasPrefix(was, "not reached") {
					log.Printf("%s: %v", addr, err)
				}
			}
			d.keep.seen[addr] = now
			d.keep.mu.Unlock()
			if n > 0 {
				log.Printf("%d entries from %s", n, addr)
			}
		}
		time.Sleep(every)
	}
}

func (d *data) machines() []string {
	if d.keep == nil {
		return nil
	}
	d.keep.mu.Lock()
	defer d.keep.mu.Unlock()
	var out []string
	for addr, how := range d.keep.seen {
		out = append(out, addr+": "+how)
	}
	sort.Strings(out)
	return out
}

// The design goes round in the log. It's the tables' own statements, so a
// file that never had a .sql file can share its design too.

// designNow is the data file's statements, and a hash that doesn't depend on
// how they came to be written.
func (l *syncLog) designNow() (stmts, hash string, err error) {
	rows, err := l.d.db.Query(`SELECT type, name, sql FROM sqlite_master WHERE sql IS NOT NULL
		AND name NOT LIKE 'sqlite%' AND name NOT LIKE '\_%' ESCAPE '\' ORDER BY type = 'table' DESC, name`)
	if err != nil {
		return "", "", err
	}
	defer rows.Close()
	var all strings.Builder
	h := sha256.New()
	for rows.Next() {
		var kind, name, stmt string
		if err := rows.Scan(&kind, &name, &stmt); err != nil {
			return "", "", err
		}
		all.WriteString(stmt + ";\n")
		if kind == "table" {
			stmt = tableBody(stmt)
		}
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00", kind, name, stmt)
	}
	return all.String(), hex.EncodeToString(h.Sum(nil)), rows.Err()
}

// latestDesign is the hash and statements of the newest design in the log.
func (l *syncLog) latestDesign() (hash, stmts string, err error) {
	var fields string
	err = l.d.db.QueryRow(`SELECT row, fields FROM _sync_entries WHERE kind = ? ORDER BY t DESC, node DESC, seq DESC LIMIT 1`, entryDesign).Scan(&hash, &fields)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	} else if err != nil {
		return "", "", err
	}
	var f map[string]string
	if err := json.Unmarshal([]byte(fields), &f); err != nil {
		return "", "", err
	}
	return hash, f["sql"], nil
}

// noteDesign puts the design in the log if it isn't the newest there.
func (l *syncLog) noteDesign() error {
	stmts, hash, err := l.designNow()
	if err != nil || stmts == "" {
		return err
	}
	latest, _, err := l.latestDesign()
	if err != nil || latest == hash {
		return err
	}
	return l.write(entryDesign, "", hash, map[string]any{"sql": stmts})
}

// catchDesign makes the tables match the newest design in the log.
func (l *syncLog) catchDesign() error {
	latest, stmts, err := l.latestDesign()
	if err != nil || latest == "" {
		return err
	}
	if _, now, err := l.designNow(); err != nil || now == latest {
		return err
	}
	f, err := os.CreateTemp("", "design-*.sql")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	f.WriteString(stmts)
	f.Close()
	return importDesign(l.d.path, f.Name(), true, io.Discard)
}

// adoptAccounts puts accounts made before the log into it.
func (l *syncLog) adoptAccounts(accounts *sql.DB) error {
	rows, err := accounts.Query(`SELECT name, hash, role FROM accounts`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name, hash, role string
		if err := rows.Scan(&name, &hash, &role); err != nil {
			return err
		}
		var n int
		if err := l.d.db.QueryRow(`SELECT COUNT(*) FROM _sync_accounts WHERE name = ?`, name).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if err := l.write(entryAccount, "", name, map[string]any{"hash": hash, "role": role}); err != nil {
				return err
			}
		}
	}
	return rows.Err()
}
