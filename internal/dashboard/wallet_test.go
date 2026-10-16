package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWallet(t *testing.T) {
	// The admin's dashboard (200::1, account "desktop") keeps the ledger;
	// the member's (200::2, account "pi") pays through messages.
	ctx, admin, member, do, _, listOf := namePair(t, nil)
	wallet := func(d *Dashboard) walletOut {
		r := httptest.NewRequest(http.MethodGet, "/api/wallet", nil)
		r.Host = "127.0.0.1:7480"
		var out walletOut
		json.Unmarshal(do(d, r).Body.Bytes(), &out)
		return out
	}
	pay := func(d *Dashboard, body string) *httptest.ResponseRecorder {
		return do(d, post("/api/wallet/pay", strings.NewReader(body)))
	}
	until := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%s: never happened", what)
			}
			admin.syncWallet(ctx, listOf(admin))
			member.syncWallet(ctx, listOf(member))
		}
	}

	if w := wallet(member); !w.On || w.Account != "pi" || w.Keeper != "desktop" || w.Keeping || len(w.Payees) != 1 || w.Payees[0] != "desktop" {
		t.Fatalf("member's wallet at first: %+v", w)
	}
	for body, want := range map[string]int{`{"to":"nobody","amount":5}`: 400, `{"to":"desktop","amount":0}`: 400,
		`{"to":"pi","amount":5}`: 400, `{"to":"desktop","amount":5000000}`: 400} {
		if w := pay(member, body); w.Code != want {
			t.Errorf("pay %s: %d %s", body, w.Code, w.Body)
		}
	}

	// The member pays: waiting, then in the ledger.
	if w := pay(member, `{"to":"desktop","amount":300,"note":"for the drive"}`); w.Code != 200 || !strings.Contains(w.Body.String(), "sent") {
		t.Fatalf("pay: %d %s", w.Code, w.Body)
	}
	if w := wallet(member); len(w.Sent) != 1 || w.Sent[0].Status != "waiting" {
		t.Fatalf("waiting payment: %+v", w)
	}
	until("the payment reaching the ledger and coming back", func() bool {
		w := wallet(member)
		return w.Balance == -300 && len(w.Sent) == 0 && len(w.History) == 1
	})
	if w := wallet(admin); !w.Keeping || w.Balance != 300 || len(w.History) != 1 || w.History[0].Note != "for the drive" || w.History[0].From != "pi" {
		t.Fatalf("keeper's wallet: %+v", w)
	}

	// The keeper pays the member back, straight into the ledger.
	if w := pay(admin, `{"to":"pi","amount":100}`); w.Code != 200 || !strings.Contains(w.Body.String(), "paid") {
		t.Fatalf("keeper pays: %d %s", w.Code, w.Body)
	}
	until("the member hearing of it", func() bool { return wallet(member).Balance == -200 })
	sum := int64(0)
	for _, b := range wallet(member).Balances {
		sum += b.Balance
	}
	if sum != 0 {
		t.Fatalf("balances add up to %d", sum)
	}

	// A payment the keeper refuses (here, to someone who left the group
	// between sending and the ledger taking it) is marked so.
	l, _ := member.msgClient()
	f := member.loadWallet()
	f.Sent = append(f.Sent, sentPayment{Status: "waiting"})
	f.Sent[0].ID, f.Sent[0].From, f.Sent[0].To, f.Sent[0].Amount = "gone1", "pi", "ghost", 5
	member.saveWallet(f)
	l.Send(context.Background(), "200::1", typeYggPay, `{"id":"gone1","to":"ghost","amount":5}`)
	until("the refusal coming back", func() bool {
		w := wallet(member)
		return len(w.Sent) == 1 && w.Sent[0].Status == "refused" && strings.Contains(w.Sent[0].Why, "no account")
	})

	// Only the keeper's word counts: a state from anyone else is ignored.
	l.Send(context.Background(), "200::2", typeYggState, `{"account":"pi","balances":{"pi":1000000},"at":9999999999}`)
	member.syncWallet(ctx, listOf(member))
	if w := wallet(member); w.Balance != -200 {
		t.Fatalf("a state not from the keeper was taken: %+v", w)
	}
}
