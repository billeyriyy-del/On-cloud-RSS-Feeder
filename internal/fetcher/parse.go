package fetcher

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"html"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mmcdole/gofeed"
	ext "github.com/mmcdole/gofeed/extensions"
	"golang.org/x/net/html/charset"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/unicode"

	"rssfeeder/internal/model"
	"rssfeeder/internal/normalize"
)

// ── Tolerant decoding ─────────────────────────────────────────────────────────
//
// Real feeds are frequently broken in boring ways. Everything here turns bytes
// into something gofeed will accept, without guessing more than necessary:
//   1. BOMs (UTF-8, UTF-16) are honoured and removed.
//   2. A non-UTF-8 charset (XML declaration first, then Content-Type) is
//      decoded to UTF-8 and the declaration rewritten to match.
//   3. Stray invalid bytes in "UTF-8" feeds are read as Windows-1252, which is
//      what a mis-declared Latin feed almost always is.
//   4. Control characters that XML forbids are dropped.
// If parsing still fails, repairXML fixes bare ampersands, HTML-only entities,
// junk before the root element and junk after it, then parsing is retried once.

var (
	xmlDeclRe = regexp.MustCompile(`^\s*<\?xml[^>]*?encoding\s*=\s*["']([A-Za-z0-9._:-]+)["'][^>]*\?>`)
	entityRe  = regexp.MustCompile(`&([A-Za-z][A-Za-z0-9]{1,31});`)
)

// toUTF8 normalises a feed body to UTF-8 (see above).
func toUTF8(body []byte, contentType string) []byte {
	switch {
	case bytes.HasPrefix(body, []byte{0xEF, 0xBB, 0xBF}):
		body = body[3:]
	case bytes.HasPrefix(body, []byte{0xFF, 0xFE}), bytes.HasPrefix(body, []byte{0xFE, 0xFF}):
		if dec, err := unicode.UTF16(unicode.BigEndian, unicode.UseBOM).NewDecoder().Bytes(body); err == nil {
			body = rewriteDecl(dec)
		}
	}

	label := ""
	if m := xmlDeclRe.FindSubmatch(body); m != nil {
		label = string(m[1])
	} else if _, params, err := mime.ParseMediaType(contentType); err == nil {
		label = params["charset"]
	}
	if label != "" && !isUTF8Label(label) {
		if enc, _ := charset.Lookup(label); enc != nil {
			if dec, err := enc.NewDecoder().Bytes(body); err == nil {
				body = rewriteDecl(dec)
			}
		}
	}
	if !utf8.Valid(body) {
		body = fixMixedUTF8(body)
	}
	return dropControlChars(body)
}

func isUTF8Label(l string) bool {
	l = strings.ToLower(l)
	return l == "utf-8" || l == "utf8" || l == "us-ascii" || l == "ascii"
}

// rewriteDecl makes the XML declaration agree with the (now UTF-8) bytes.
func rewriteDecl(b []byte) []byte {
	if loc := xmlDeclRe.FindSubmatchIndex(b); loc != nil {
		out := make([]byte, 0, len(b))
		out = append(out, b[:loc[2]]...)
		out = append(out, "UTF-8"...)
		return append(out, b[loc[3]:]...)
	}
	return b
}

// fixMixedUTF8 keeps valid UTF-8 sequences and decodes each invalid byte as Windows-1252.
func fixMixedUTF8(b []byte) []byte {
	var out bytes.Buffer
	out.Grow(len(b) + len(b)/8)
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size <= 1 {
			out.WriteRune(charmap.Windows1252.DecodeByte(b[0]))
			b = b[1:]
			continue
		}
		out.Write(b[:size])
		b = b[size:]
	}
	return out.Bytes()
}

// dropControlChars removes C0 controls other than tab/LF/CR, and U+FFFE/U+FFFF.
func dropControlChars(b []byte) []byte {
	clean := true
	for _, c := range b {
		if c < 0x20 && c != '\t' && c != '\n' && c != '\r' {
			clean = false
			break
		}
	}
	if clean && !bytes.Contains(b, []byte("￾")) && !bytes.Contains(b, []byte("￿")) {
		return b
	}
	return bytes.Map(func(r rune) rune {
		if (r < 0x20 && r != '\t' && r != '\n' && r != '\r') || r == 0xFFFE || r == 0xFFFF {
			return -1
		}
		return r
	}, b)
}

var xmlEntities = map[string]bool{"amp": true, "lt": true, "gt": true, "quot": true, "apos": true}

// repairXML fixes the common ways hand-rolled feeds break XML well-formedness.
func repairXML(b []byte) []byte {
	s := string(b)
	// Junk before the document (PHP notices, blank lines before <?xml).
	if i := strings.Index(s, "<?xml"); i > 0 {
		s = s[i:]
	} else if i < 0 {
		if j := strings.IndexByte(s, '<'); j > 0 {
			s = s[j:]
		}
	}
	// Junk after the root element.
	for _, end := range []string{"</rss>", "</feed>", "</rdf:RDF>", "</RDF>"} {
		if i := strings.LastIndex(s, end); i >= 0 {
			s = s[:i+len(end)]
			break
		}
	}
	// HTML entities XML does not define: &nbsp; → &#160;
	s = entityRe.ReplaceAllStringFunc(s, func(m string) string {
		name := m[1 : len(m)-1]
		if xmlEntities[name] {
			return m
		}
		if u := html.UnescapeString(m); u != m {
			var out strings.Builder
			for _, r := range u {
				fmt.Fprintf(&out, "&#%d;", r)
			}
			return out.String()
		}
		return "&amp;" + m[1:]
	})
	// Bare ampersands that do not start a reference.
	var out strings.Builder
	out.Grow(len(s) + 64)
	for i := 0; i < len(s); i++ {
		if s[i] == '&' && !startsReference(s[i+1:]) {
			out.WriteString("&amp;")
			continue
		}
		out.WriteByte(s[i])
	}
	return []byte(out.String())
}

func startsReference(s string) bool {
	end := strings.IndexByte(s, ';')
	if end <= 0 || end > 32 {
		return false
	}
	ref := s[:end]
	if ref[0] == '#' {
		ref = ref[1:]
		if ref == "" {
			return false
		}
		if ref[0] == 'x' || ref[0] == 'X' {
			_, err := strconv.ParseUint(ref[1:], 16, 32)
			return err == nil
		}
		_, err := strconv.ParseUint(ref, 10, 32)
		return err == nil
	}
	for i, c := range ref {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// ErrNotFeed means the document parsed as neither XML nor JSON feed.
var ErrNotFeed = errors.New("not a feed")

// ParseFeed decodes and parses any feed format gofeed supports (RSS 0.9x/1.0/
// 2.0, Atom 0.3/1.0, JSON Feed 1.0/1.1), repairing broken XML when needed.
func ParseFeed(body []byte, contentType string) (*gofeed.Feed, error) {
	body = toUTF8(body, contentType)
	p := gofeed.NewParser()
	feed, err := p.Parse(bytes.NewReader(body))
	if err == nil {
		return feed, nil
	}
	if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 && trimmed[0] == '{' {
		return nil, fmt.Errorf("json feed: %w", err)
	}
	if looksLikeHTML(body) {
		return nil, ErrNotFeed
	}
	if feed, err2 := p.Parse(bytes.NewReader(repairXML(body))); err2 == nil {
		Metrics.Add("feeds_repaired", 1)
		return feed, nil
	}
	if errors.Is(err, gofeed.ErrFeedTypeNotDetected) {
		return nil, ErrNotFeed
	}
	return nil, err
}

func looksLikeHTML(b []byte) bool {
	head := strings.ToLower(string(b[:min(len(b), 1024)]))
	return strings.Contains(head, "<!doctype html") || strings.Contains(head, "<html")
}

// ── Entry → Item ──────────────────────────────────────────────────────────────

// maxFutureSkew: publishers with broken clocks or scheduled posts would pin
// items to the top of every timeline; anything further ahead is clamped.
const maxFutureSkew = 24 * 3600

// BuildItems normalises every entry of a parsed feed into Items and derives
// feed-level metadata. feedURL is the URL the feed was fetched from.
func BuildItems(sourceID int64, feed *gofeed.Feed, feedURL string, now int64) ([]*model.Item, model.FeedMeta) {
	siteURL := normalize.ResolveURL(feedURL, feed.Link)
	if siteURL == feedURL {
		siteURL = ""
	}
	base := siteURL
	if base == "" {
		base = feedURL
	}
	feedImage := ""
	if feed.Image != nil {
		feedImage = normalize.ResolveURL(base, feed.Image.URL)
	}
	if feedImage == "" && feed.ITunesExt != nil {
		feedImage = normalize.ResolveURL(base, feed.ITunesExt.Image)
	}
	feedAuthor := ""
	if feed.Author != nil {
		feedAuthor = feed.Author.Name
	} else if feed.ITunesExt != nil {
		feedAuthor = feed.ITunesExt.Author
	}
	lang := strings.ToLower(strings.TrimSpace(feed.Language))

	var items []*model.Item
	kinds := map[model.ItemKind]int{}
	for _, e := range feed.Items {
		it := entryToItem(sourceID, e, base, feedImage, feedAuthor, lang, now)
		if it == nil {
			continue
		}
		kinds[it.Kind]++
		items = append(items, it)
	}
	meta := model.FeedMeta{
		Title:       normalize.CleanTitle(feed.Title),
		SiteURL:     siteURL,
		IconURL:     feedImage,
		Description: normalize.Truncate(normalize.CleanTitle(feed.Description), 300),
		Kind:        model.KindArticle,
	}
	for k, n := range kinds {
		if n*2 > len(items) {
			meta.Kind = k
		}
	}
	return items, meta
}

func entryToItem(sourceID int64, e *gofeed.Item, base, feedImage, feedAuthor, lang string, now int64) *model.Item {
	link := e.Link
	if link == "" && len(e.Links) > 0 {
		link = e.Links[0]
	}
	if link == "" && (strings.HasPrefix(e.GUID, "http://") || strings.HasPrefix(e.GUID, "https://")) {
		link = e.GUID
	}
	link = normalize.StripTrackingParams(normalize.ResolveURL(base, link))
	itemBase := link
	if itemBase == "" {
		itemBase = base
	}
	baseURL, _ := url.Parse(itemBase)

	// Content: prefer full content, then description; YouTube and podcasts
	// often only have media:description / itunes:summary.
	raw := e.Content
	if raw == "" {
		raw = e.Description
	}
	mediaDesc := mediaText(e.Extensions, "description")
	if raw == "" && mediaDesc != "" {
		raw = normalize.TextToHTML(mediaDesc)
	}
	if raw == "" && e.ITunesExt != nil {
		raw = e.ITunesExt.Summary
		if !strings.Contains(raw, "<") {
			raw = normalize.TextToHTML(raw)
		}
	}
	content := normalize.SanitizeHTML(normalize.ResolveURLs(raw, baseURL))
	text := normalize.HTMLToText(content)

	title := normalize.CleanTitle(e.Title)
	if title == "" {
		title = normalize.Truncate(normalize.FirstSentence(text), 80)
	}
	if title == "" {
		title = "Untitled"
	}

	// Identity: GUID, else link, else title+date. Hashed so length is bounded.
	idRaw := e.GUID
	if idRaw == "" {
		idRaw = link
	}
	if idRaw == "" {
		if e.Title == "" && text == "" {
			return nil
		}
		idRaw = e.Title + "|" + e.Published + "|" + normalize.Truncate(text, 200)
	}
	guid := fmt.Sprintf("%x", sha256.Sum256([]byte(idRaw)))

	published := now
	switch {
	case e.PublishedParsed != nil:
		published = e.PublishedParsed.Unix()
	case e.UpdatedParsed != nil:
		published = e.UpdatedParsed.Unix()
	}
	if published > now+maxFutureSkew || published < 0 {
		published = now
	}
	var entryUpd int64
	if e.UpdatedParsed != nil {
		entryUpd = e.UpdatedParsed.Unix()
	}

	author := ""
	switch {
	case e.Author != nil && e.Author.Name != "":
		author = e.Author.Name
	case len(e.Authors) > 0 && e.Authors[0] != nil:
		author = e.Authors[0].Name
	case e.ITunesExt != nil && e.ITunesExt.Author != "":
		author = e.ITunesExt.Author
	case e.DublinCoreExt != nil && len(e.DublinCoreExt.Creator) > 0:
		author = e.DublinCoreExt.Creator[0]
	default:
		author = feedAuthor
	}
	author = normalize.CleanTitle(author)

	// Summary: a distinct description reads better than the first lines of the
	// body; podcasts' itunes:subtitle is written to be a summary.
	summarySrc := text
	if e.Description != "" && e.Content != "" && e.Description != e.Content {
		summarySrc = normalize.HTMLToText(e.Description)
	}
	if e.ITunesExt != nil && e.ITunesExt.Subtitle != "" {
		summarySrc = normalize.CleanTitle(e.ITunesExt.Subtitle)
	}
	summary := normalize.Truncate(summarySrc, 280)

	encs := enclosures(e, baseURL)
	kind := kindOf(link, encs, e.Extensions)

	image := ""
	switch {
	case e.Image != nil && e.Image.URL != "" && !strings.HasPrefix(e.Image.URL, "data:"):
		image = e.Image.URL // note: gofeed may take this from the first <img> in the body
	case e.ITunesExt != nil && e.ITunesExt.Image != "":
		image = e.ITunesExt.Image
	}
	if image == "" {
		image = mediaAttr(e.Extensions, "thumbnail", "url")
	}
	if image == "" {
		image = mediaImage(e.Extensions)
	}
	if image == "" {
		for _, enc := range encs {
			if strings.HasPrefix(enc.Type, "image/") {
				image = enc.URL
				break
			}
		}
	}
	if image == "" {
		image = normalize.FirstImage(content)
	}
	if image == "" && kind != model.KindArticle {
		image = feedImage // podcast artwork
	}
	image = normalize.ResolveURL(itemBase, image)

	reading := normalize.ReadingSecs(text)
	if kind == model.KindAudio || kind == model.KindVideo {
		for _, enc := range encs {
			if enc.Duration > 0 {
				reading = enc.Duration
				break
			}
		}
	}

	return &model.Item{
		SourceID:    sourceID,
		GUID:        guid,
		URL:         link,
		Title:       title,
		Author:      author,
		Summary:     summary,
		ImageURL:    image,
		Kind:        kind,
		Lang:        lang,
		ReadingSecs: reading,
		PublishedAt: published,
		FetchedAt:   now,
		EntryUpdAt:  entryUpd,
		ContentHTML: content,
		ContentText: text,
		Enclosures:  encs,
	}
}

func enclosures(e *gofeed.Item, base *url.URL) []*model.Enclosure {
	var out []*model.Enclosure
	seen := map[string]bool{}
	add := func(u, typ, length string) {
		u = strings.TrimSpace(u)
		if u == "" {
			return
		}
		if base != nil {
			if ref, err := url.Parse(u); err == nil {
				u = base.ResolveReference(ref).String()
			}
		}
		if seen[u] {
			return
		}
		seen[u] = true
		if typ == "" {
			typ = mime.TypeByExtension(path.Ext(strings.SplitN(u, "?", 2)[0]))
		}
		n, _ := strconv.ParseInt(strings.TrimSpace(length), 10, 64)
		out = append(out, &model.Enclosure{URL: u, Type: typ, Length: n})
	}
	for _, enc := range e.Enclosures {
		if enc != nil {
			add(enc.URL, enc.Type, enc.Length)
		}
	}
	// media:content (Media RSS) for audio/video when there is no enclosure.
	if len(out) == 0 {
		for _, m := range mediaNodes(e.Extensions, "content") {
			if medium := m.Attrs["medium"]; medium == "audio" || medium == "video" ||
				strings.HasPrefix(m.Attrs["type"], "audio/") || strings.HasPrefix(m.Attrs["type"], "video/") {
				add(m.Attrs["url"], m.Attrs["type"], m.Attrs["fileSize"])
			}
		}
	}
	if e.ITunesExt != nil && e.ITunesExt.Duration != "" {
		if d := parseDuration(e.ITunesExt.Duration); d > 0 {
			for _, enc := range out {
				if strings.HasPrefix(enc.Type, "audio/") || strings.HasPrefix(enc.Type, "video/") {
					enc.Duration = d
					break
				}
			}
		}
	}
	return out
}

func kindOf(link string, encs []*model.Enclosure, exts ext.Extensions) model.ItemKind {
	if _, ok := exts["yt"]; ok {
		return model.KindVideo
	}
	if u, err := url.Parse(link); err == nil {
		h := strings.TrimPrefix(u.Hostname(), "www.")
		if (h == "youtube.com" || h == "m.youtube.com") && (u.Path == "/watch" || strings.HasPrefix(u.Path, "/shorts/")) || h == "youtu.be" || h == "vimeo.com" {
			return model.KindVideo
		}
	}
	for _, e := range encs {
		switch {
		case strings.HasPrefix(e.Type, "audio/"):
			return model.KindAudio
		case strings.HasPrefix(e.Type, "video/"):
			return model.KindVideo
		}
	}
	return model.KindArticle
}

// parseDuration reads itunes:duration: "3600", "59:59", "1:02:03" or "1:02:03.5".
func parseDuration(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if !strings.Contains(s, ":") {
		f, _ := strconv.ParseFloat(s, 64)
		return int64(f)
	}
	var total int64
	for _, p := range strings.Split(s, ":") {
		f, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return 0
		}
		total = total*60 + int64(f)
	}
	return total
}

// Media RSS helpers: nodes may sit directly on the item or inside media:group.
func mediaNodes(exts ext.Extensions, name string) []ext.Extension {
	media := exts["media"]
	if media == nil {
		return nil
	}
	out := append([]ext.Extension{}, media[name]...)
	for _, g := range media["group"] {
		out = append(out, g.Children[name]...)
	}
	return out
}

func mediaAttr(exts ext.Extensions, name, attr string) string {
	for _, n := range mediaNodes(exts, name) {
		if v := n.Attrs[attr]; v != "" {
			return v
		}
	}
	return ""
}

func mediaText(exts ext.Extensions, name string) string {
	for _, n := range mediaNodes(exts, name) {
		if v := strings.TrimSpace(n.Value); v != "" {
			return v
		}
	}
	return ""
}

func mediaImage(exts ext.Extensions) string {
	for _, n := range mediaNodes(exts, "content") {
		if n.Attrs["medium"] == "image" || strings.HasPrefix(n.Attrs["type"], "image/") {
			return n.Attrs["url"]
		}
	}
	return ""
}

// ── WebSub discovery ──────────────────────────────────────────────────────────

var (
	linkTagRe = regexp.MustCompile(`(?is)<(?:atom:|atom10:)?link\b[^>]*>`)
	relRe     = regexp.MustCompile(`(?i)\brel\s*=\s*["']([^"']+)["']`)
	hrefRe    = regexp.MustCompile(`(?i)\bhref\s*=\s*["']([^"']+)["']`)
	headerRe  = regexp.MustCompile(`<([^>]+)>\s*;[^,]*?rel\s*=\s*"?([^";,]+)"?`)
)

// findHub returns the WebSub hub and self (topic) URLs a feed advertises, via
// HTTP Link headers or <link rel="hub"> / <link rel="self"> in the document.
func findHub(body []byte, header http.Header) (hub, self string) {
	for _, v := range header.Values("Link") {
		for _, m := range headerRe.FindAllStringSubmatch(v, -1) {
			for _, rel := range strings.Fields(strings.ToLower(m[2])) {
				if rel == "hub" && hub == "" {
					hub = m[1]
				}
				if rel == "self" && self == "" {
					self = m[1]
				}
			}
		}
	}
	head := body[:min(len(body), 16<<10)]
	for _, tag := range linkTagRe.FindAll(head, 32) {
		rel, href := relRe.FindSubmatch(tag), hrefRe.FindSubmatch(tag)
		if rel == nil || href == nil {
			continue
		}
		v := html.UnescapeString(string(href[1]))
		switch strings.ToLower(string(rel[1])) {
		case "hub":
			if hub == "" {
				hub = v
			}
		case "self":
			if self == "" {
				self = v
			}
		}
	}
	return hub, self
}

// jitter returns d ± up to 10%, so feeds added together do not stay in lockstep.
func jitter(d int64) int64 {
	if d < 10 {
		return d
	}
	return d - d/10 + time.Now().UnixNano()%(d/5+1)
}
