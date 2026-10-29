package site

import (
	"net/url"
	"strings"
)

// The ygg button the dashboard puts on a member's home page when they turn
// it on (see docs/ygg.md, The ygg button), between markers like the contact
// link. It opens the visitor's own dashboard, which asks before giving:
// the page itself can't give anything.
const (
	yggButtonStart = "<!-- yggstore: ygg button -->"
	yggButtonEnd   = "<!-- /yggstore: ygg button -->"
)

// YggGivePage is where a member's own dashboard asks before giving.
const YggGivePage = "http://localhost:7480/ygg"

func yggButtonHTML(site string) string {
	href := YggGivePage + "?to=" + url.QueryEscape(Normalise(site))
	return yggButtonStart + "\n<p style=\"text-align:center;margin:2em 0\"><a href=\"" + href +
		"\" title=\"Members of the group can give this site yggs from their own dashboard\">Give a ygg</a></p>\n" +
		yggButtonEnd + "\n"
}

// WithYggButton adds the button for site to a home page before </body>,
// in place of one added before.
func WithYggButton(page, site string) string {
	page = WithoutYggButton(page)
	if i := strings.LastIndex(strings.ToLower(page), "</body>"); i >= 0 {
		return page[:i] + yggButtonHTML(site) + page[i:]
	}
	return page + yggButtonHTML(site)
}

// WithoutYggButton takes out the button WithYggButton added.
func WithoutYggButton(page string) string {
	return withoutMarked(page, yggButtonStart, yggButtonEnd)
}

// withoutMarked takes out what lies between start and end, the markers
// included.
func withoutMarked(page, start, end string) string {
	i := strings.Index(page, start)
	if i < 0 {
		return page
	}
	j := strings.Index(page[i:], end)
	if j < 0 {
		return page
	}
	j += i + len(end)
	if j < len(page) && page[j] == '\n' {
		j++
	}
	return page[:i] + page[j:]
}
