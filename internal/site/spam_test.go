package site

import (
	"strings"
	"testing"
	"time"
)

func TestSpamScore(t *testing.T) {
	for _, c := range []struct {
		what   string
		fields []field
		marked bool
	}{
		{"random letters from a throwaway address", []field{{"name", "Bob"}, {"email", "2ry~e95v0d4d2y@merepost.com"}, {"subject", "s0z7jz"}, {"message", "if355b"}}, true},
		{"a pitch with WordPress's field", []field{{"ak_js", "1793646512531"}, {"email", "support@decentgears.example"},
			{"message", "We have curated a stunning collection of luxury-inspired bags. Explore it at www.decentgears.example."}}, true},
		{"an SEO pitch", []field{{"name", "Sam"}, {"email", "sam@seo.example"},
			{"message", "I used your contact form to say: our SEO gets you on the first page of Google. See https://seo.example"}}, true},
		{"a customer about marketing, with their site", []field{{"name", "Anna"}, {"email", "anna@shop.example"},
			{"message", "We'd like bulk SMS for our marketing and leads. Our shop is https://shop.example. Can you quote?"}}, false},
		{"a blog comment with a porn link in HTML", []field{{"name", "Louvenia"}, {"email", "lou@mail.example"}, {"subject", "About Messagemate"},
			{"message", "Its noot my first time to pay a quick visit this ite (<a href=\"https://go.xvideos.example/\">here</a>), i am viisiting this weeb site dailly."}}, true},
		{"a blog comment without links", []field{{"name", "Adrianna"}, {"email", "a@mail.example"},
			{"message", "My brother recommended I woyld like this web site. This post actually made my day. Keep up the writing!"}}, false},
		{"the feedback template", []field{{"name", "Diana Doyle"}, {"email", "anita8cda@alazsec.example"}, {"subject", "Feedback regarding your services"},
			{"message", "one of my hobbies is poetry. and when i'm writing poems this works great."}}, true},
		{"a blog compliment", []field{{"name", "Steve"}, {"email", "steve@mail.example"}, {"subject", "About Messagemate"},
			{"message", "I love your blog.. very nice colors & theme. Did you design this website yourself? I'm looking to design my own blog."}}, true},
		{"a price question in another language", []field{{"name", "Robertirobe"}, {"message", "Kaixo, zure prezioa jakin nahi nuen."}}, false},
		{"a short quote request", []field{{"name", "Thabo"}, {"message", "Please quote me for 5,000 SMSes a month"}}, false},
		{"a short real message", []field{{"name", "Thabo"}, {"email", "thabo@mail.example"}, {"message", "Please call me"}}, false},
		{"a link back to the site", []field{{"name", "Lee"}, {"message", "About https://messagemate.co.za/pricing: is VAT included?"}}, false},
	} {
		score, why := suspicion("messagemate.co.za", c.fields)
		if (score >= spamMark) != c.marked {
			t.Errorf("%s: score %d (%v), marked %v", c.what, score, why, !c.marked)
		}
	}
	raw := string(contactMessage("example.org", "", "192.0.2.1", "/", []field{{"email", "x@merepost.com"}, {"message", "dmgqde"}, {"subject", "pey0kf"}}, time.Now()))
	if !strings.Contains(raw, "Subject: [Possible spam] [example.org]") || !strings.Contains(raw, "X-Yggstore-Spam: possible") ||
		!strings.Contains(raw, "Marked as possible spam") {
		t.Fatalf("not marked:\n%s", raw)
	}
	raw = string(contactMessage("example.org", "", "192.0.2.1", "/", []field{{"name", "Anna"}, {"message", "Hello there"}}, time.Now()))
	if strings.Contains(raw, "Possible spam") || strings.Contains(raw, "X-Yggstore-Spam") {
		t.Fatalf("a real message marked:\n%s", raw)
	}
}
