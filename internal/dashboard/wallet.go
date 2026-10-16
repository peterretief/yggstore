package dashboard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/atomicfile"
	"github.com/peterretief/yggstore/internal/msg"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/ygg"
)

// The wallet (see docs/ygg.md): each member's balance in yggs, and paying
// other members. An account is a person, the owner the group's list gives a
// node (peers.Peer.Operator), so all of someone's nodes share one.
//
// The group's admin node keeps the ledger (package ygg); its dashboard takes
// payments. Other dashboards reach it by direct message, and a message says
// which node sent it, so a node can only pay from its own owner's account.

const (
	typeYggPay   = "ygg-pay"   // member → keeper: yggPayReq
	typeYggAsk   = "ygg-ask"   // member → keeper: {}, send me my wallet
	typeYggState = "ygg-state" // keeper → member: walletView
)

// DefaultWalletLimit is how far below 0 a balance may go before the wallet
// warns: 50,000 yggs, $50. For now it only warns.
const DefaultWalletLimit = 50_000

// walletHistory is how many of an account's payments the keeper sends.
const walletHistory = 100

type yggPayReq struct {
	ID     string `json:"id"`
	To     string `json:"to"`
	Amount int64  `json:"amount"`
	Note   string `json:"note,omitempty"`
}

// walletView is an account's part of the ledger: what the keeper sends a
// member's nodes, and what the page shows.
type walletView struct {
	Account  string            `json:"account"`
	Balances map[string]int64  `json:"balances"` // everyone's: open to the group
	History  []ygg.Payment     `json:"history"`  // the account's own, newest first
	Refused  map[string]string `json:"refused,omitempty"`
	At       int64             `json:"at"` // unix seconds
}

// sentPayment is a payment sent to the keeper, until the ledger has it.
type sentPayment struct {
	ygg.Payment
	Sent   int64  `json:"sent"` // unix seconds
	Status string `json:"status"`
	Why    string `json:"why,omitempty"`
}

type walletFile struct {
	After int64         `json:"after"` // last message read
	View  *walletView   `json:"view,omitempty"`
	Sent  []sentPayment `json:"sent"`
	Asked int64         `json:"asked"` // unix seconds of the last ask
}

func (d *Dashboard) walletPath() string { return filepath.Join(d.cfg.WalletDir, "wallet.json") }

func (d *Dashboard) loadWallet() walletFile {
	var f walletFile
	if b, err := os.ReadFile(d.walletPath()); err == nil {
		json.Unmarshal(b, &f)
	}
	return f
}

func (d *Dashboard) saveWallet(f walletFile) {
	b, _ := json.MarshalIndent(f, "", "  ")
	err := os.MkdirAll(d.cfg.WalletDir, 0o700)
	if err == nil {
		err = atomicfile.Replace(d.walletPath(), append(b, '\n'), 0o600)
	}
	if err != nil {
		d.event("warn", "could not save the wallet: "+err.Error())
	}
}

// keeperOf is the node that keeps the ledger: the first admin in the list.
func keeperOf(list []peers.Peer) (peers.Peer, bool) {
	for _, p := range list {
		if p.Admin {
			return p, true
		}
	}
	return peers.Peer{}, false
}

// accountOf is the account of the node at ip.
func accountOf(list []peers.Peer, ip string) string {
	if p, ok := selfEntry(list, ip); ok {
		return p.Operator()
	}
	return ""
}

// walletAccounts are everyone in the group with an account.
func walletAccounts(list []peers.Peer) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, p := range list {
		if a := p.Operator(); a != "" && !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	sort.Strings(out)
	return out
}

// nodesOf are the machines of account's nodes, other than this one.
func (d *Dashboard) nodesOf(list []peers.Peer, account string) []string {
	seen := map[string]bool{d.cfg.SelfID: true}
	out := []string{}
	for _, p := range list {
		if ip := p.IP(); p.Operator() == account && !seen[ip] {
			seen[ip] = true
			out = append(out, ip)
		}
	}
	return out
}

// ledger is the ledger, opened the first time this dashboard keeps it.
func (d *Dashboard) ledger() (*ygg.Ledger, error) {
	if d.yggLedger == nil {
		l, err := ygg.Open(filepath.Join(d.cfg.WalletDir, "ledger"))
		if err != nil {
			return nil, err
		}
		d.yggLedger = l
	}
	return d.yggLedger, nil
}

// view is account's part of the ledger, as the keeper has it.
func (d *Dashboard) view(lg *ygg.Ledger, list []peers.Peer, account string) walletView {
	v := walletView{Account: account, Balances: lg.Balances(), History: lg.History(account, walletHistory), At: time.Now().Unix()}
	for _, a := range walletAccounts(list) {
		v.Balances[a] += 0 // everyone, at 0 until they pay or are paid
	}
	return v
}

// tell sends account's nodes their view.
func (d *Dashboard) tell(ctx context.Context, l msg.Local, lg *ygg.Ledger, list []peers.Peer, account string) {
	b, _ := json.Marshal(d.view(lg, list, account))
	for _, ip := range d.nodesOf(list, account) {
		if _, err := l.Send(ctx, ip, typeYggState, string(b)); err != nil {
			d.event("warn", fmt.Sprintf("wallet: could not tell %s: %v", ip, err))
		}
	}
}

// pay takes a payment from the node at from into the ledger, here at the
// keeper, and tells both people's nodes.
func (d *Dashboard) pay(ctx context.Context, l msg.Local, list []peers.Peer, from string, req yggPayReq) (ygg.Payment, error) {
	lg, err := d.ledger()
	if err != nil {
		return ygg.Payment{}, err
	}
	p := ygg.Payment{ID: req.ID, From: accountOf(list, from), To: strings.TrimSpace(req.To), Amount: req.Amount, Note: strings.TrimSpace(req.Note)}
	if p.From == "" {
		return ygg.Payment{}, errors.New("that node isn't in the group's list")
	}
	if !contains(walletAccounts(list), p.To) {
		return ygg.Payment{}, fmt.Errorf("%q has no account in the group", p.To)
	}
	p, taken, err := lg.Pay(p)
	if err != nil {
		return ygg.Payment{}, err
	}
	if taken {
		d.event("info", fmt.Sprintf("wallet: %s paid %s %d yggs (%s)", p.From, p.To, p.Amount, ygg.Dollars(p.Amount)))
	}
	d.tell(ctx, l, lg, list, p.From)
	d.tell(ctx, l, lg, list, p.To)
	return p, nil
}

// syncWallet reads what came in for the wallet: at the keeper, payments and
// asks; elsewhere, the keeper's answers. poll calls it.
func (d *Dashboard) syncWallet(ctx context.Context, list []peers.Peer) {
	if d.cfg.WalletDir == "" {
		return
	}
	l, err := d.msgClient()
	if err != nil {
		return
	}
	d.walletMu.Lock()
	defer d.walletMu.Unlock()
	f := d.loadWallet()
	keeper, hasKeeper := keeperOf(list)
	keeping := hasKeeper && keeper.IP() == d.cfg.SelfID
	mctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	msgs, err := l.Messages(mctx, f.After, "direct", 1000)
	if err != nil {
		return
	}
	changed := false
	for _, m := range msgs {
		f.After, changed = max(f.After, m.N), true
		switch m.Type {
		case typeYggPay:
			if !keeping {
				continue
			}
			var req yggPayReq
			if err := json.Unmarshal([]byte(m.Body), &req); err != nil {
				continue
			}
			if _, err := d.pay(mctx, l, list, m.From, req); err != nil {
				lg, lerr := d.ledger()
				if lerr != nil {
					continue
				}
				v := d.view(lg, list, accountOf(list, m.From))
				v.Refused = map[string]string{req.ID: err.Error()}
				b, _ := json.Marshal(v)
				l.Send(mctx, m.From, typeYggState, string(b))
			}
		case typeYggAsk:
			if lg, err := d.ledger(); keeping && err == nil {
				if a := accountOf(list, m.From); a != "" {
					b, _ := json.Marshal(d.view(lg, list, a))
					l.Send(mctx, m.From, typeYggState, string(b))
				}
			}
		case typeYggState:
			if !hasKeeper || m.From != keeper.IP() {
				continue // only the keeper's word counts
			}
			var v walletView
			if err := json.Unmarshal([]byte(m.Body), &v); err != nil || v.Account != accountOf(list, d.cfg.SelfID) {
				continue
			}
			if f.View == nil || v.At >= f.View.At {
				refused := v.Refused
				v.Refused = nil
				f.View = &v
				f.Sent = settle(f.Sent, v.History, refused)
			} else {
				f.Sent = settle(f.Sent, nil, v.Refused)
			}
		}
	}
	if hasKeeper && !keeping && time.Now().Unix()-f.Asked > 3600 {
		f.Asked, changed = time.Now().Unix(), true
		l.Send(mctx, keeper.IP(), typeYggAsk, "{}")
	}
	if changed {
		d.saveWallet(f)
	}
}

// settle drops sent payments the ledger now has, and marks those refused.
func settle(sent []sentPayment, history []ygg.Payment, refused map[string]string) []sentPayment {
	in := map[string]bool{}
	for _, p := range history {
		in[p.ID] = true
	}
	out := []sentPayment{}
	for _, s := range sent {
		if in[s.ID] {
			continue
		}
		if why, ok := refused[s.ID]; ok {
			s.Status, s.Why = "refused", why
		}
		out = append(out, s)
	}
	return out
}

func newPaymentID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

type walletBalance struct {
	Account string `json:"account"`
	Balance int64  `json:"balance"`
	Me      bool   `json:"me,omitempty"`
}

type walletOut struct {
	On       bool            `json:"on"`
	Error    string          `json:"error,omitempty"`
	Account  string          `json:"account"`
	Keeper   string          `json:"keeper,omitempty"` // the node keeping the ledger
	Keeping  bool            `json:"keeping,omitempty"`
	Balance  int64           `json:"balance"`
	Balances []walletBalance `json:"balances"`
	History  []ygg.Payment   `json:"history"`
	Sent     []sentPayment   `json:"sent"`
	Payees   []string        `json:"payees"` // accounts you can pay
	Limit    int64           `json:"limit"`
	At       int64           `json:"at"` // when the keeper last said, unix seconds; 0 if never
}

func (d *Dashboard) walletLimit() int64 {
	if d.cfg.WalletLimit > 0 {
		return d.cfg.WalletLimit
	}
	return DefaultWalletLimit
}

func (d *Dashboard) handleWallet(w http.ResponseWriter, r *http.Request) {
	out := walletOut{Balances: []walletBalance{}, History: []ygg.Payment{}, Sent: []sentPayment{}, Payees: []string{}, Limit: d.walletLimit()}
	if d.cfg.WalletDir == "" {
		out.Error = "this dashboard has no wallet"
		writeJSONResp(w, out)
		return
	}
	list, err := peers.Load(d.cfg.PeersPath)
	if err != nil {
		out.Error = err.Error()
		writeJSONResp(w, out)
		return
	}
	out.Account = accountOf(list, d.cfg.SelfID)
	keeper, hasKeeper := keeperOf(list)
	switch {
	case out.Account == "":
		out.Error = "this node isn't in the group's list, so it has no account"
	case !hasKeeper:
		out.Error = "the group's list has no admin to keep the ledger"
	}
	if out.Error != "" {
		writeJSONResp(w, out)
		return
	}
	out.On, out.Keeper, out.Keeping = true, keeper.Name, keeper.IP() == d.cfg.SelfID
	for _, a := range walletAccounts(list) {
		if a != out.Account {
			out.Payees = append(out.Payees, a)
		}
	}
	d.walletMu.Lock()
	defer d.walletMu.Unlock()
	f := d.loadWallet()
	var v *walletView
	if out.Keeping {
		lg, err := d.ledger()
		if err != nil {
			out.Error = "the ledger: " + err.Error()
			writeJSONResp(w, out)
			return
		}
		vv := d.view(lg, list, out.Account)
		v = &vv
	} else {
		v = f.View
		// Looking at the wallet asks the keeper for news, at most twice a
		// minute.
		if now := time.Now().Unix(); now-f.Asked > 30 {
			if l, err := d.msgClient(); err == nil {
				ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
				if _, err := l.Send(ctx, keeper.IP(), typeYggAsk, "{}"); err == nil {
					f.Asked = now
					d.saveWallet(f)
				}
				cancel()
			}
		}
	}
	if v != nil {
		out.Balance, out.History, out.At = v.Balances[out.Account], v.History, v.At
		for a, b := range v.Balances {
			out.Balances = append(out.Balances, walletBalance{Account: a, Balance: b, Me: a == out.Account})
		}
		sort.Slice(out.Balances, func(i, j int) bool { return out.Balances[i].Account < out.Balances[j].Account })
	}
	if f.Sent != nil {
		out.Sent = f.Sent
	}
	writeJSONResp(w, out)
}

func (d *Dashboard) handleWalletPay(w http.ResponseWriter, r *http.Request) {
	var req yggPayReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if d.cfg.WalletDir == "" {
		http.Error(w, "this dashboard has no wallet", http.StatusConflict)
		return
	}
	list, err := peers.Load(d.cfg.PeersPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	keeper, ok := keeperOf(list)
	if !ok {
		http.Error(w, "the group's list has no admin to keep the ledger", http.StatusConflict)
		return
	}
	req.ID, req.To, req.Note = newPaymentID(), strings.TrimSpace(req.To), strings.TrimSpace(req.Note)
	p := ygg.Payment{ID: req.ID, From: accountOf(list, d.cfg.SelfID), To: req.To, Amount: req.Amount, Note: req.Note}
	if err := p.Check(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !contains(walletAccounts(list), p.To) {
		http.Error(w, fmt.Sprintf("%q has no account in the group", p.To), http.StatusBadRequest)
		return
	}
	l, err := d.msgClient()
	if err != nil {
		http.Error(w, "messaging: "+err.Error(), http.StatusConflict)
		return
	}
	d.walletMu.Lock()
	defer d.walletMu.Unlock()
	if keeper.IP() == d.cfg.SelfID {
		if _, err := d.pay(r.Context(), l, list, d.cfg.SelfID, req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSONResp(w, map[string]string{"status": "paid"})
		return
	}
	b, _ := json.Marshal(req)
	if _, err := l.Send(r.Context(), keeper.IP(), typeYggPay, string(b)); err != nil {
		http.Error(w, "could not send it to "+keeper.Name+", which keeps the ledger: "+err.Error(), http.StatusBadGateway)
		return
	}
	f := d.loadWallet()
	f.Sent = append(f.Sent, sentPayment{Payment: p, Sent: time.Now().Unix(), Status: "waiting"})
	d.saveWallet(f)
	d.event("info", fmt.Sprintf("wallet: sent %s %d yggs (%s), waiting for %s to record it", p.To, p.Amount, ygg.Dollars(p.Amount), keeper.Name))
	writeJSONResp(w, map[string]string{"status": "sent"})
}
