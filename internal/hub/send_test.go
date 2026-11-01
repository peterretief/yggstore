package hub

import (
	"strings"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/mail"
)

func TestSendLimits(t *testing.T) {
	now := time.Now()
	s := HubState{People: []Person{{Name: "tommy", Trial: &Trial{Email: "t@example.net"}}, {Name: "kath"}}}
	if err := s.reserve("tommy", TrialSendPerMessage+1, now); err == nil {
		t.Fatal("a trial message to too many people")
	}
	for range TrialSendPerDay / TrialSendPerMessage {
		if err := s.reserve("tommy", TrialSendPerMessage, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.reserve("tommy", 1, now); err == nil || !strings.Contains(err.Error(), "the most you may") {
		t.Fatalf("over the day's limit: %v", err)
	}
	if err := s.reserve("tommy", 1, now.Add(25*time.Hour)); err != nil {
		t.Fatalf("a day later: %v", err)
	}
	// Someone the box's owner added has more room.
	if err := s.reserve("kath", SendPerMessage, now); err != nil {
		t.Fatal(err)
	}
	s.People[0].Trial.Leaving = now.Unix()
	if err := s.reserve("tommy", 1, now.Add(25*time.Hour)); err == nil {
		t.Fatal("sent after leaving")
	}
}

func TestSendRecipientsAndHeaders(t *testing.T) {
	if _, err := recipients("anna@example.net, not an address"); err == nil {
		t.Fatal("took a bad address")
	}
	got, err := recipients("Anna <Anna@Example.net>, bob@example.net")
	if err != nil || strings.Join(got, ",") != "anna@example.net,bob@example.net" {
		t.Fatalf("%v %v", got, err)
	}
	// A subject can't add headers of its own.
	raw, rcpts, err := mail.Draft{From: "tommy@example.org", To: "anna@example.net", Subject: "hi\r\nBcc: victim@example.net", Text: "x"}.Compose()
	if err != nil {
		t.Fatal(err)
	}
	if len(rcpts) != 1 || strings.Contains(string(raw), "\r\nBcc:") {
		t.Fatalf("a header slipped in: %v\n%s", rcpts, raw)
	}
}
