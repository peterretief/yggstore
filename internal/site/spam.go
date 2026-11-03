package site

import (
	"net/mail"
	"regexp"
	"strings"
)

// Contact form messages that look like spam but may be a person's are
// still delivered, marked: their subject starts with "[Possible spam]" and
// an X-Yggstore-Spam header and a line at the end say why, so the owner
// (and their mail program's filters) can tell. A post that is surely a
// robot's is dropped instead (junk, in contact.go).
const spamMark = 3 // a message scoring this much is marked

var (
	disposable = map[string]bool{
		"merepost.com": true, "mailinator.com": true, "guerrillamail.com": true, "sharklasers.com": true,
		"yopmail.com": true, "temp-mail.org": true, "10minutemail.com": true, "trashmail.com": true,
		"getnada.com": true, "dispostable.com": true, "maildrop.cc": true, "tempmail.com": true,
	}
	// Field names a robot adds, copied from WordPress plugins' forms, which
	// no group site has.
	botFields = []string{"ak_js", "ak_hp", "ak_bib", "ak_fp", "_wpcf7", "wpforms", "g-recaptcha"}
	// What sales pitches say, and real enquiries hardly ever do.
	pitches = []string{"seo", "backlink", "search engine", "first page of google", "rank higher", "used your contact form",
		"your contact form", "lead generation", "web design", "redesign your website", "guest post", "luxury", "replica",
		"crypto", "bitcoin", "casino", "loan", "credit available", "unsubscribe", "opt out", "opt-out", "limited time"}
	linkRe     = regexp.MustCompile(`(?i)\b(?:https?://|www\.)([a-z0-9.-]+\.[a-z]{2,})`)
	gibberish  = regexp.MustCompile(`^[a-z0-9]{4,12}$`)
	hasDigit   = regexp.MustCompile(`[0-9]`)
	hasVowel   = regexp.MustCompile(`[aeiouy]`)
	pitchWords = func() []*regexp.Regexp {
		var out []*regexp.Regexp
		for _, p := range pitches {
			out = append(out, regexp.MustCompile(`\b`+regexp.QuoteMeta(p)+`\b`))
		}
		return out
	}()
)

// suspicion scores a post to site, with the reasons.
func suspicion(site string, fields []field) (int, []string) {
	score, why := 0, []string{}
	add := func(n int, reason string) { score, why = score+n, append(why, reason) }
	var message strings.Builder
	odd := 0
	for _, f := range fields {
		n, v := strings.ToLower(f.name), strings.TrimSpace(f.value)
		for _, b := range botFields {
			if strings.HasPrefix(n, b) {
				add(2, "a field from another site's form ("+f.name+")")
				break
			}
		}
		if strings.HasPrefix(n, "_") {
			continue
		}
		if n == "email" {
			if a, err := mail.ParseAddress(v); err == nil {
				if _, domain, _ := strings.Cut(strings.ToLower(a.Address), "@"); disposable[domain] {
					add(2, "a throwaway email address")
				}
			}
			continue
		}
		if strings.Contains(n, "phone") || strings.Contains(n, "tel") {
			continue
		}
		// A word of random letters and digits ("pey0kf") where a person
		// writes words.
		lv := strings.ToLower(v)
		if gibberish.MatchString(lv) && (hasDigit.MatchString(lv) || !hasVowel.MatchString(lv)) {
			odd++
		}
		message.WriteString(lv + "\n")
	}
	if odd > 0 {
		add(min(odd, 2), "random letters where words go")
	}
	text := message.String()
	for _, m := range linkRe.FindAllStringSubmatch(text, -1) {
		d := strings.TrimPrefix(strings.ToLower(m[1]), "www.")
		if d != strings.TrimPrefix(site, "www.") && !strings.HasSuffix(d, "."+strings.TrimPrefix(site, "www.")) {
			add(1, "a link to another site")
			break
		}
	}
	matched := 0
	for i, re := range pitchWords {
		if re.MatchString(text) {
			matched++
			if matched == 1 {
				why = append(why, "sales words ("+pitches[i]+")")
			}
		}
	}
	score += min(matched, 2)
	return score, why
}
