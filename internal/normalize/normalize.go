// Package normalize turns messy publisher HTML and URLs into the clean,
// predictable values Noema stores.
package normalize

import (
	"bytes"
	"html"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/microcosm-cc/bluemonday"
	xhtml "golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

var (
	// trackingRe matches query-parameter keys that are tracking noise.
	trackingRe = regexp.MustCompile(`(?i)^(utm_|fbclid$|gclid$|msclkid$|yclid$|mc_eid$|mc_cid$|ref$|ref_src$|_hsenc$|_hsmi$|igshid$|mkt_tok$)`)

	ugcPolicy = newPolicy()

	tagRe = regexp.MustCompile(`<[^>]*>`)
	// Inline elements vanish without a space, so "<a>world</a>." stays "world.".
	inlineTagRe = regexp.MustCompile(`(?i)</?(?:a|em|strong|span|b|i|u|s|code|abbr|mark|sub|sup|small|del|ins|q|cite|time|font|kbd|var)\b[^>]*>`)
	spaceRe     = regexp.MustCompile(`[\s\p{Zs}\x{200B}\x{FEFF}]+`) // incl. NBSP, zero-width space
)

// newPolicy is bluemonday's UGC policy plus what feeds legitimately need:
// figures, responsive images, audio/video with controls, and lazy loading.
func newPolicy() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowElements("figure", "figcaption", "picture", "source", "mark", "small", "sub", "sup", "details", "summary")
	p.AllowAttrs("srcset", "sizes", "media", "type").OnElements("img", "source")
	p.AllowAttrs("loading", "decoding").Matching(regexp.MustCompile(`^(lazy|eager|async|auto|sync)$`)).OnElements("img")
	p.AllowElements("audio", "video")
	p.AllowAttrs("src", "poster", "preload").OnElements("audio", "video", "source")
	p.AllowAttrs("controls", "playsinline", "muted", "loop").OnElements("audio", "video")
	p.RequireNoReferrerOnLinks(true)
	p.AddTargetBlankToFullyQualifiedLinks(true)
	return p
}

// StripTrackingParams removes well-known tracking query parameters from rawURL.
// Returns rawURL unchanged on parse error.
func StripTrackingParams(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.RawQuery == "" {
		return rawURL
	}
	q := u.Query()
	changed := false
	for key := range q {
		if trackingRe.MatchString(key) {
			q.Del(key)
			changed = true
		}
	}
	if !changed {
		return rawURL
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// SanitizeHTML runs rawHTML through the reader policy.
func SanitizeHTML(rawHTML string) string {
	return ugcPolicy.Sanitize(rawHTML)
}

// HTMLToText converts sanitized HTML to plain text suitable for FTS indexing.
func HTMLToText(sanitized string) string {
	text := inlineTagRe.ReplaceAllString(sanitized, "")
	text = tagRe.ReplaceAllString(text, " ")
	text = html.UnescapeString(text)
	return strings.TrimSpace(spaceRe.ReplaceAllString(text, " "))
}

// CleanTitle strips markup and entities (some feeds double-escape titles) and
// collapses whitespace.
func CleanTitle(s string) string {
	s = HTMLToText(s)
	if strings.Contains(s, "&") {
		s = html.UnescapeString(s) // double-escaped: "&amp;amp;" → "&amp;" → "&"
	}
	return strings.TrimSpace(s)
}

// FirstSentence returns the first sentence of text (or all of it).
func FirstSentence(text string) string {
	if loc := sentenceRe.FindStringIndex(text); loc != nil {
		return strings.TrimSpace(text[:loc[0]+len(strings.TrimRight(text[loc[0]:loc[1]], " \t\n"))])
	}
	return text
}

// Truncate shortens text to at most n runes, cutting at a word boundary when
// one is near, and appends an ellipsis if anything was removed.
func Truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)[:n]
	cut := len(r)
	for i := len(r) - 1; i > n*3/4; i-- {
		if unicode.IsSpace(r[i]) {
			cut = i
			break
		}
	}
	return strings.TrimRightFunc(string(r[:cut]), func(c rune) bool {
		return unicode.IsSpace(c) || unicode.IsPunct(c)
	}) + "…"
}

// ReadingSecs estimates reading time: ~230 words/min for space-separated
// scripts, ~400 characters/min for CJK (which has no spaces to count).
func ReadingSecs(text string) int64 {
	var words, cjk int
	inWord := false
	for _, r := range text {
		switch {
		case isCJK(r):
			cjk++
			inWord = false
		case unicode.IsLetter(r) || unicode.IsNumber(r):
			if !inWord {
				words++
				inWord = true
			}
		default:
			inWord = false
		}
	}
	return int64(float64(words)/230*60 + float64(cjk)/400*60 + 0.5)
}

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r)
}

// ResolveURLs rewrites relative href/src/srcset/poster URLs in an HTML fragment
// against base, promotes lazy-load attributes (data-src, data-srcset) to real
// ones, and returns the re-rendered fragment. On parse failure it returns the input.
func ResolveURLs(fragment string, base *url.URL) string {
	if base == nil || fragment == "" {
		return fragment
	}
	ctx := &xhtml.Node{Type: xhtml.ElementNode, Data: "div", DataAtom: atom.Div}
	nodes, err := xhtml.ParseFragment(strings.NewReader(fragment), ctx)
	if err != nil {
		return fragment
	}
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode {
			promoteLazy(n)
			for i, a := range n.Attr {
				switch a.Key {
				case "href", "src", "poster", "cite":
					n.Attr[i].Val = resolve(base, a.Val)
				case "srcset":
					n.Attr[i].Val = resolveSrcset(base, a.Val)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	var buf bytes.Buffer
	for _, n := range nodes {
		walk(n)
		if err := xhtml.Render(&buf, n); err != nil {
			return fragment
		}
	}
	return buf.String()
}

func promoteLazy(n *xhtml.Node) {
	if n.Data != "img" && n.Data != "source" {
		return
	}
	get := func(k string) (int, string) {
		for i, a := range n.Attr {
			if a.Key == k {
				return i, a.Val
			}
		}
		return -1, ""
	}
	for _, pair := range [][2]string{{"data-src", "src"}, {"data-lazy-src", "src"}, {"data-srcset", "srcset"}} {
		if _, lazy := get(pair[0]); lazy != "" {
			i, cur := get(pair[1])
			if cur == "" || strings.HasPrefix(cur, "data:") {
				if i >= 0 {
					n.Attr[i].Val = lazy
				} else {
					n.Attr = append(n.Attr, xhtml.Attribute{Key: pair[1], Val: lazy})
				}
			}
		}
	}
}

func resolve(base *url.URL, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.HasPrefix(ref, "#") || strings.HasPrefix(ref, "data:") || strings.HasPrefix(ref, "mailto:") {
		return ref
	}
	u, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return base.ResolveReference(u).String()
}

func resolveSrcset(base *url.URL, v string) string {
	parts := strings.Split(v, ",")
	for i, p := range parts {
		f := strings.Fields(strings.TrimSpace(p))
		if len(f) == 0 {
			continue
		}
		f[0] = resolve(base, f[0])
		parts[i] = strings.Join(f, " ")
	}
	return strings.Join(parts, ", ")
}

// ResolveURL resolves ref against base (either may be empty).
func ResolveURL(base, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	b, err := url.Parse(base)
	if err != nil || base == "" {
		return ref
	}
	return resolve(b, ref)
}

var imgSrcRe = regexp.MustCompile(`(?i)<img[^>]+src="([^"]+)"`)

// FirstImage returns the first <img src> in sanitized HTML, skipping tracking
// pixels and emoji sprites where the markup says so.
func FirstImage(sanitized string) string {
	for _, m := range imgSrcRe.FindAllStringSubmatch(sanitized, 8) {
		src := html.UnescapeString(m[1])
		low := strings.ToLower(src)
		if strings.HasPrefix(low, "data:") || strings.Contains(low, "feedburner") || strings.Contains(low, "pixel") ||
			strings.Contains(low, "/emoji/") || strings.Contains(low, "gravatar.com") {
			continue
		}
		return src
	}
	return ""
}

// TextToHTML turns plain text (e.g. a YouTube description) into safe paragraphs.
func TextToHTML(text string) string {
	var b strings.Builder
	for _, para := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n\n") {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		b.WriteString("<p>")
		b.WriteString(strings.ReplaceAll(html.EscapeString(para), "\n", "<br>"))
		b.WriteString("</p>")
	}
	return b.String()
}

var (
	blockEndRe = regexp.MustCompile(`(?i)</(p|div|h[1-6]|li|blockquote|pre|figure|figcaption|tr|section|article)>|<br\s*/?>|<hr\s*/?>`)
	sentenceRe = regexp.MustCompile(`([.!?。！？]["'”’)]?)\s+`)
)

// SpeechChunks turns article HTML into utterances for text-to-speech: one per
// paragraph, small paragraphs merged and long ones split at sentence ends, so
// each chunk stays under max runes (speech engines stall on very long input).
func SpeechChunks(sanitized string, max int) []string {
	text := blockEndRe.ReplaceAllString(sanitized, "\n\n")
	text = tagRe.ReplaceAllString(text, " ")
	text = html.UnescapeString(text)
	var paras []string
	for _, p := range strings.Split(text, "\n\n") {
		p = strings.TrimSpace(spaceRe.ReplaceAllString(p, " "))
		if p != "" {
			paras = append(paras, p)
		}
	}
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, p := range paras {
		for _, piece := range splitSentences(p, max) {
			if cur.Len() > 0 && utf8.RuneCountInString(cur.String())+1+utf8.RuneCountInString(piece) > max {
				flush()
			}
			if cur.Len() > 0 {
				cur.WriteByte(' ')
			}
			cur.WriteString(piece)
		}
		if utf8.RuneCountInString(cur.String()) > max/2 {
			flush()
		}
	}
	flush()
	return out
}

func splitSentences(p string, max int) []string {
	if utf8.RuneCountInString(p) <= max {
		return []string{p}
	}
	marked := sentenceRe.ReplaceAllString(p, "$1\x00")
	var out []string
	for _, s := range strings.Split(marked, "\x00") {
		s = strings.TrimSpace(s)
		for utf8.RuneCountInString(s) > max { // no sentence breaks: hard wrap
			r := []rune(s)
			out = append(out, string(r[:max]))
			s = string(r[max:])
		}
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
