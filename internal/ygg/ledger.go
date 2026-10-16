// Package ygg keeps the group's ledger of yggs (see docs/ygg.md): who paid
// whom how many, from which each member's balance follows. A ygg is
// $0.001. Balances start at 0 and always add up to 0, since every payment
// takes from one member what it gives another. Nothing in it is money held
// for anyone, and nothing can be cashed out of it.
//
// One node keeps the ledger, the group's admin. It is plain JSON lines, a
// file per day, so anyone can add it up again.
package ygg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// MaxAmount is the most one payment can be: 1,000,000 yggs, $1,000.
	MaxAmount = 1_000_000
	// MaxNote is the longest note, in characters.
	MaxNote = 200
)

// KindStorage marks the keeper's daily payment for storage: the person
// whose files were held pays the holder.
const KindStorage = "storage"

// Payment is one payment between two members' accounts. Accounts are the
// people the group's list says run its nodes (peers.Peer.Operator).
type Payment struct {
	ID     string `json:"id"`             // chosen by the payer's dashboard, so a retry isn't paid twice
	Kind   string `json:"kind,omitempty"` // "" for a payment a member made, or KindStorage
	From   string `json:"from"`
	To     string `json:"to"`
	Amount int64  `json:"amount"` // yggs
	Note   string `json:"note,omitempty"`
	At     int64  `json:"at"` // unix seconds, when the ledger took it
}

// Check reports what's wrong with a payment before it is taken.
func (p Payment) Check() error {
	switch {
	case p.ID == "" || len(p.ID) > 64:
		return errors.New("the payment has no proper ID")
	case p.From == "" || p.To == "":
		return errors.New("the payment needs who pays and who is paid")
	case p.From == p.To:
		return errors.New("you can't pay yourself")
	case p.Amount < 1:
		return errors.New("pay at least 1 ygg")
	case p.Amount > MaxAmount:
		return fmt.Errorf("one payment can be at most %d yggs ($%d)", MaxAmount, MaxAmount/1000)
	case utf8.RuneCountInString(p.Note) > MaxNote:
		return fmt.Errorf("the note can be at most %d characters", MaxNote)
	}
	return nil
}

// Ledger is the ledger in a directory.
type Ledger struct {
	dir string

	mu       sync.Mutex
	payments []Payment
	ids      map[string]int // index in payments
}

// Open reads the ledger in dir, making dir if it's missing.
func Open(dir string) (*Ledger, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	l := &Ledger{dir: dir, ids: map[string]int{}}
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files) // by day
	for _, f := range files {
		if err := l.read(f); err != nil {
			return nil, err
		}
	}
	return l, nil
}

func (l *Ledger) read(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	good, n := 0, 0 // bytes up to the end of the last whole line, and lines read
	for len(b[good:]) > 0 {
		end := bytes.IndexByte(b[good:], '\n')
		if end < 0 {
			break // cut short by a crash: dropped below
		}
		line := bytes.TrimSpace(b[good : good+end])
		good += end + 1
		n++
		if len(line) == 0 {
			continue
		}
		var p Payment
		if err := json.Unmarshal(line, &p); err != nil {
			return fmt.Errorf("%s line %d: %w", path, n, err)
		}
		if _, dup := l.ids[p.ID]; !dup {
			l.ids[p.ID] = len(l.payments)
			l.payments = append(l.payments, p)
		}
	}
	if good < len(b) {
		// A payment half written when the machine stopped was never
		// confirmed to anyone: drop it, so the next one starts a clean line.
		return os.Truncate(path, int64(good))
	}
	return nil
}

// Pay takes p, unless the ledger has it already, in which case it gives back
// the one it has (taken is false). At is set to now.
func (l *Ledger) Pay(p Payment) (got Payment, taken bool, err error) {
	if err := p.Check(); err != nil {
		return Payment{}, false, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if i, ok := l.ids[p.ID]; ok {
		if old := l.payments[i]; old.From != p.From {
			return Payment{}, false, errors.New("that payment ID is someone else's")
		}
		return l.payments[i], false, nil
	}
	now := time.Now().UTC()
	p.At = now.Unix()
	b, err := json.Marshal(p)
	if err != nil {
		return Payment{}, false, err
	}
	f, err := os.OpenFile(filepath.Join(l.dir, now.Format("2006-01-02")+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return Payment{}, false, err
	}
	_, err = f.Write(append(b, '\n'))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return Payment{}, false, err
	}
	l.ids[p.ID] = len(l.payments)
	l.payments = append(l.payments, p)
	return p, true, nil
}

// Balances are every account's balance, in yggs.
func (l *Ledger) Balances() map[string]int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := map[string]int64{}
	for _, p := range l.payments {
		b[p.From] -= p.Amount
		b[p.To] += p.Amount
	}
	return b
}

// History is the latest payments to or from account, newest first, at most
// limit of them.
func (l *Ledger) History(account string, limit int) []Payment {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []Payment{}
	for i := len(l.payments) - 1; i >= 0 && len(out) < limit; i-- {
		if p := l.payments[i]; p.From == account || p.To == account {
			out = append(out, p)
		}
	}
	return out
}

// Dollars shows yggs as US dollars: 1250 is "$1.25".
func Dollars(yggs int64) string {
	sign := ""
	if yggs < 0 {
		sign, yggs = "-", -yggs
	}
	cents := (yggs + 5) / 10 // to the nearest cent
	return fmt.Sprintf("%s$%d.%02d", sign, cents/100, cents%100)
}
