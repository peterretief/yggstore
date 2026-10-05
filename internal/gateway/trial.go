package gateway

import (
	"fmt"
	"time"
)

// TrialGrace is how long a trial customer can still download (and delete)
// their files after the trial ends, before the account stops working.
const TrialGrace = 30 * 24 * time.Hour

// DefaultTrialQuota is a trial's space when none is given.
const DefaultTrialQuota = 5e9

// Access is what an account may do right now.
type Access int

const (
	AccessNone     Access = iota
	AccessReadOnly        // download, list and delete, but not upload
	AccessFull
)

// Access says what c may do at now, and why when it is limited.
func (c Customer) Access(now time.Time) (Access, string) {
	switch {
	case c.Closed != 0:
		return AccessNone, "this account is closed"
	case c.Suspended:
		return AccessNone, "this account is suspended; contact the group's organiser"
	case c.TrialEnds == 0 || now.Unix() < c.TrialEnds:
		return AccessFull, ""
	}
	ended := time.Unix(c.TrialEnds, 0)
	until := ended.Add(TrialGrace)
	if now.Before(until) {
		return AccessReadOnly, fmt.Sprintf("your free trial ended on %s. You can still download your files until %s; contact the group's organiser to keep using the service",
			day(ended), day(until))
	}
	return AccessNone, fmt.Sprintf("your free trial ended on %s; contact the group's organiser", day(ended))
}

// Status describes the account for the organiser.
func (c Customer) Status(now time.Time) string {
	switch {
	case c.Closed != 0:
		return "closed " + day(time.Unix(c.Closed, 0))
	case c.Suspended:
		return "suspended"
	case c.TrialEnds == 0:
		return "active"
	}
	ends := time.Unix(c.TrialEnds, 0)
	if left := ends.Sub(now); left > 0 {
		days := int(left.Hours()/24 + 0.999)
		return fmt.Sprintf("trial, %d day%s left (ends %s)", days, plural(days), day(ends))
	}
	if until := ends.Add(TrialGrace); now.Before(until) {
		return fmt.Sprintf("trial ended %s: download only until %s", day(ends), day(until))
	}
	return fmt.Sprintf("trial ended %s: no access (close it to free the space)", day(ends))
}

func day(t time.Time) string { return t.Local().Format("2 Jan 2006") }

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
