package githubsync

import (
	"bytes"
	"io"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/microcosm-cc/bluemonday"
	"golang.org/x/net/html"
)

const (
	// MaxBodyHTML is the largest sanitised body SanitizeHTML returns.
	MaxBodyHTML = 256 << 10
	// SummaryLength is the number of runes Summary keeps before it adds "…".
	SummaryLength = 280

	// summaryHardCut is how far back from SummaryLength Summary looks for a
	// space before it cuts mid-word.
	summaryHardCut = 40
)

// imageSrc are the image URLs GitHub puts in rendered bodies. On github.com
// itself only uploaded attachments are allowed, so a body can't make
// readers' browsers request other github.com pages.
var imageSrc = regexp.MustCompile(`^https://((camo|user-images|avatars|raw)\.githubusercontent\.com|github\.com/user-attachments/assets)/`)

// privateUserImage is how GitHub renders an uploaded attachment: a signed
// URL that expires five minutes after the body is fetched, and differs on
// every fetch. The UUID names the same file at
// https://github.com/user-attachments/assets/<uuid>, as in the markdown body.
var privateUserImage = regexp.MustCompile(`https://private-user-images\.githubusercontent\.com/[0-9]+/[0-9]+-([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})(?:\.[A-Za-z0-9]+)?(?:\?[^"'\s<>]*)?`)

// stableAttachments replaces expiring attachment URLs with their permanent
// form, so a stored body keeps working and the same issue always sanitises
// to the same HTML.
func stableAttachments(body string) string {
	return privateUserImage.ReplaceAllString(body, "https://github.com/user-attachments/assets/$1")
}

// bodyPolicy is the allow-list for issue bodies rendered by GitHub. A built
// policy is safe for concurrent use.
var bodyPolicy = newBodyPolicy()

func newBodyPolicy() *bluemonday.Policy {
	p := bluemonday.NewPolicy()

	p.AllowElements(
		"p", "br", "hr", "h1", "h2", "h3", "h4", "h5", "h6",
		"blockquote", "pre", "code",
		"em", "strong", "b", "i", "del", "s", "ins", "sub", "sup", "kbd", "mark", "small",
		"ul", "ol", "li", "dl", "dt", "dd",
		"table", "thead", "tbody", "tfoot", "tr", "th", "td",
		"details", "summary", "div", "span",
	)
	digits := regexp.MustCompile(`^[0-9]+$`)
	p.AllowAttrs("start").Matching(digits).OnElements("ol")
	p.AllowAttrs("align").Matching(regexp.MustCompile(`^(left|center|right)$`)).OnElements("th", "td")
	p.AllowAttrs("open").OnElements("details")

	// Links: absolute http, https and mailto URLs only. Every link gets
	// rel="nofollow noreferrer"; links with a host also get target="_blank",
	// which makes bluemonday add noopener.
	p.AllowAttrs("href").OnElements("a")
	p.AllowURLSchemes("http", "https", "mailto")
	p.RequireParseableURLs(true)
	p.AllowRelativeURLs(false)
	p.RequireNoFollowOnLinks(true)
	p.RequireNoReferrerOnLinks(true)
	p.AddTargetBlankToFullyQualifiedLinks(true)

	// Images: https only, and only from GitHub, so a post never makes a
	// reader's browser fetch from anywhere else. GitHub proxies external
	// images through camo.githubusercontent.com and serves uploads from
	// github.com/user-attachments (see stableAttachments). fixTags drops an
	// img whose src was removed.
	p.AllowAttrs("src").Matching(imageSrc).OnElements("img")
	p.AllowAttrs("alt", "title").OnElements("img")
	p.AllowAttrs("width", "height").Matching(digits).OnElements("img")

	// Task-list checkboxes. bluemonday can't require an attribute, so
	// fixTags drops any input that isn't type="checkbox" and makes the rest
	// disabled.
	p.AllowAttrs("type").Matching(regexp.MustCompile(`^checkbox$`)).OnElements("input")
	p.AllowAttrs("checked", "disabled").OnElements("input")

	// Drop these with their content. script, style, iframe, object and a few
	// others are skipped by default; svg (GitHub's octicons) and math would
	// leave only noise behind.
	p.SkipElementsContent("svg", "math")

	return p
}

// SanitizeHTML returns GitHub's rendered issue body (Issue.bodyHTML) cleaned
// against the allow-list above. Elements not on the list are dropped and
// their text kept; script and style are dropped with their content.
//
// It returns "" when the sanitised HTML is longer than MaxBodyHTML, rather
// than cut it mid-tag; callers then store only the summary.
func SanitizeHTML(body string) string {
	out := bodyPolicy.Sanitize(stableAttachments(body))
	if strings.Contains(out, "<input") || strings.Contains(out, "<img") {
		out = fixTags(out)
	}
	if len(out) > MaxBodyHTML {
		return ""
	}
	return out
}

// fixTags does what bluemonday can't, since it can't require an attribute.
// It drops an img without a src, and an input that isn't type="checkbox"; a
// checkbox becomes <input type="checkbox" disabled=""/>, with checked="" if it
// had it. Everything else is copied as is.
func fixTags(s string) string {
	var b bytes.Buffer
	z := html.NewTokenizer(strings.NewReader(s))
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			if z.Err() == io.EOF {
				return b.String()
			}
			// The tokenizer gave up; keep nothing rather than guess.
			return ""
		}
		if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
			b.Write(z.Raw())
			continue
		}
		raw := append([]byte(nil), z.Raw()...)
		tok := z.Token()
		var src, checkbox, checked bool
		for _, a := range tok.Attr {
			switch a.Key {
			case "src":
				src = true
			case "type":
				checkbox = a.Val == "checkbox"
			case "checked":
				checked = true
			}
		}
		switch tok.Data {
		case "img":
			if src {
				b.Write(raw)
			}
			continue
		case "input":
		default:
			b.Write(raw)
			continue
		}
		if !checkbox {
			continue
		}
		b.WriteString(`<input type="checkbox"`)
		if checked {
			b.WriteString(` checked=""`)
		}
		b.WriteString(` disabled=""/>`)
	}
}

// Summary returns a plain-text excerpt of GitHub's Issue.bodyText. Runs of
// whitespace become one space, control characters and invalid UTF-8 are
// dropped, and text longer than SummaryLength runes is cut at the last space
// in the final 40 runes (or at SummaryLength if there is none) and ends in
// "…". The result is at most SummaryLength+1 runes.
func Summary(text string) string {
	runes := make([]rune, 0, min(len(text), 4*SummaryLength))
	space := false
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		i += size
		switch {
		case r == utf8.RuneError && size == 1:
			continue
		case unicode.IsSpace(r):
			space = true
			continue
		case unicode.IsControl(r):
			continue
		}
		if space && len(runes) > 0 {
			runes = append(runes, ' ')
		}
		space = false
		runes = append(runes, r)
		if len(runes) > SummaryLength+1 {
			// Enough to know it's long and where to cut.
			break
		}
	}
	if len(runes) <= SummaryLength {
		return string(runes)
	}

	cut := SummaryLength
	for i := SummaryLength; i >= SummaryLength-summaryHardCut; i-- {
		if runes[i] == ' ' {
			cut = i
			break
		}
	}
	return strings.TrimRight(string(runes[:cut]), " ") + "…"
}
