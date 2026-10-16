package ygg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLedger(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	pay := func(id, from, to string, amount int64) (Payment, bool, error) {
		return l.Pay(Payment{ID: id, From: from, To: to, Amount: amount, Note: "for " + id})
	}
	if _, taken, err := pay("a", "anna", "ben", 300); err != nil || !taken {
		t.Fatal(taken, err)
	}
	if _, _, err := pay("b", "ben", "anna", 200); err != nil {
		t.Fatal(err)
	}
	if p, taken, err := pay("a", "anna", "ben", 300); err != nil || taken || p.Amount != 300 {
		t.Fatalf("a retry: %+v %v %v", p, taken, err)
	}
	if _, _, err := pay("a", "ben", "anna", 1); err == nil {
		t.Fatal("someone else's payment ID was taken")
	}
	for _, bad := range []Payment{
		{ID: "x", From: "anna", To: "anna", Amount: 1},
		{ID: "x", From: "anna", To: "ben", Amount: 0},
		{ID: "x", From: "anna", To: "ben", Amount: MaxAmount + 1},
		{ID: "x", From: "anna", To: "ben", Amount: 1, Note: strings.Repeat("é", MaxNote+1)},
		{From: "anna", To: "ben", Amount: 1},
	} {
		if _, _, err := l.Pay(bad); err == nil {
			t.Errorf("took %+v", bad)
		}
	}
	check := func(l *Ledger) {
		t.Helper()
		b := l.Balances()
		if b["anna"] != -100 || b["ben"] != 100 || b["anna"]+b["ben"] != 0 {
			t.Fatalf("balances %v", b)
		}
		if h := l.History("anna", 10); len(h) != 2 || h[0].ID != "b" {
			t.Fatalf("history %+v", h)
		}
		if h := l.History("anna", 1); len(h) != 1 {
			t.Fatalf("limited history %+v", h)
		}
	}
	check(l)

	// Read back from disk, with a line cut short at the end, as by a crash.
	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("files %v", files)
	}
	f, _ := os.OpenFile(files[0], os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"id":"c","from":"an`)
	f.Close()
	l2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	check(l2)
	if _, _, err := l2.Pay(Payment{ID: "d", From: "ben", To: "anna", Amount: 1}); err != nil {
		t.Fatal(err)
	}
	l3, err := Open(dir)
	if err != nil {
		t.Fatalf("after a payment following a cut-short line: %v", err)
	}
	if b := l3.Balances(); b["ben"] != 99 {
		t.Fatalf("balances %v", b)
	}
}

func TestDollars(t *testing.T) {
	for yggs, want := range map[int64]string{0: "$0.00", 1: "$0.00", 5: "$0.01", 1250: "$1.25", 10: "$0.01", -1250: "-$1.25", 1_000_000: "$1000.00"} {
		if got := Dollars(yggs); got != want {
			t.Errorf("Dollars(%d) = %s, want %s", yggs, got, want)
		}
	}
}
