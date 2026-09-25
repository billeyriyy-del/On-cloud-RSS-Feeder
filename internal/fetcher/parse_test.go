package fetcher

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/text/encoding/charmap"

	"rssfeeder/internal/model"
)

const now = 1_700_000_000

func mustParse(t *testing.T, body []byte, ct string) ([]*model.Item, model.FeedMeta) {
	t.Helper()
	feed, err := ParseFeed(body, ct)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return BuildItems(1, feed, "https://example.com/feed.xml", now)
}

func TestBrokenFeedsParse(t *testing.T) {
	latin1, _ := charmap.ISO8859_1.NewEncoder().String(`<?xml version="1.0" encoding="ISO-8859-1"?>
<rss version="2.0"><channel><title>Café</title><item><title>Crème brûlée</title><link>/a</link></item></channel></rss>`)
	mislabelled, _ := charmap.Windows1252.NewEncoder().String(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><title>x</title><item><title>naïve “quotes”</title><link>https://example.com/b</link></item></channel></rss>`)

	cases := []struct {
		name, body, ct, wantTitle string
	}{
		{"utf8 BOM", "\xEF\xBB\xBF" + `<?xml version="1.0"?><rss version="2.0"><channel><title>t</title><item><title>BOM ok</title><link>https://example.com/1</link></item></channel></rss>`, "", "BOM ok"},
		{"control chars", "<rss version=\"2.0\"><channel><title>t</title><item><title>Bell\x07 and\x0B tab</title><link>https://example.com/2</link></item></channel></rss>", "", "Bell and tab"},
		{"declared latin-1", latin1, "", "Crème brûlée"},
		{"utf-8 declared, cp1252 bytes", mislabelled, "", "naïve “quotes”"},
		{"charset only in header", string(mustEncode(t, `<rss version="2.0"><channel><title>t</title><item><title>Ünïcödé</title><link>https://example.com/3</link></item></channel></rss>`)), "application/rss+xml; charset=iso-8859-1", "Ünïcödé"},
		{"bare ampersand + html entity", `<rss version="2.0"><channel><title>t</title><item><title>Fish & Chips&nbsp;&mdash; AT&T</title><link>https://example.com/4?a=1&b=2</link></item></channel></rss>`, "", "Fish & Chips — AT&T"},
		{"junk around document", "Warning: PHP notice\n<?xml version=\"1.0\"?><rss version=\"2.0\"><channel><title>t</title><item><title>Junk ok</title><link>https://example.com/5</link></item></channel></rss>\n<!-- cache 0.2s -->garbage", "", "Junk ok"},
		{"double-escaped title", `<rss version="2.0"><channel><title>t</title><item><title>Tom &amp;amp; Jerry &lt;b&gt;bold&lt;/b&gt;</title><link>https://example.com/6</link></item></channel></rss>`, "", "Tom & Jerry bold"},
		{"atom", `<feed xmlns="http://www.w3.org/2005/Atom"><title>A</title><entry><title type="html">Atom &lt;em&gt;entry&lt;/em&gt;</title><id>urn:1</id><link href="https://example.com/7"/><updated>2023-11-01T00:00:00Z</updated></entry></feed>`, "", "Atom entry"},
		{"json feed", `{"version":"https://jsonfeed.org/version/1.1","title":"J","items":[{"id":"1","url":"https://example.com/8","title":"JSON ok","content_html":"<p>hi</p>"}]}`, "application/feed+json", "JSON ok"},
		{"rss 1.0 rdf", `<?xml version="1.0"?><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns="http://purl.org/rss/1.0/"><channel rdf:about="x"><title>R</title></channel><item rdf:about="https://example.com/9"><title>RDF ok</title><link>https://example.com/9</link></item></rdf:RDF>`, "", "RDF ok"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			items, _ := mustParse(t, []byte(c.body), c.ct)
			if len(items) != 1 {
				t.Fatalf("items = %d", len(items))
			}
			if items[0].Title != c.wantTitle {
				t.Fatalf("title = %q, want %q", items[0].Title, c.wantTitle)
			}
		})
	}
}

func mustEncode(t *testing.T, s string) []byte {
	b, err := charmap.ISO8859_1.NewEncoder().Bytes([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestHTMLIsNotAFeed(t *testing.T) {
	_, err := ParseFeed([]byte(`<!DOCTYPE html><html><head><title>x</title></head><body>hi</body></html>`), "text/html")
	if err != ErrNotFeed {
		t.Fatalf("err = %v, want ErrNotFeed", err)
	}
}

func TestPodcastEpisode(t *testing.T) {
	body := `<rss version="2.0" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd"><channel>
<title>Pod</title><language>en-NZ</language><itunes:image href="https://cdn.example.com/show.jpg"/>
<item><title>Ep 1</title><guid isPermaLink="false">ep1</guid>
<enclosure url="/audio/ep1.mp3" length="1234" type="audio/mpeg"/>
<itunes:duration>1:02:03</itunes:duration><itunes:subtitle>A short subtitle</itunes:subtitle>
<itunes:summary>Long summary text</itunes:summary></item></channel></rss>`
	items, meta := mustParse(t, []byte(body), "")
	it := items[0]
	if it.Kind != model.KindAudio || meta.Kind != model.KindAudio {
		t.Fatalf("kind = %s / feed %s", it.Kind, meta.Kind)
	}
	if len(it.Enclosures) != 1 || it.Enclosures[0].URL != "https://example.com/audio/ep1.mp3" || it.Enclosures[0].Duration != 3723 {
		t.Fatalf("enclosure = %+v", it.Enclosures[0])
	}
	if it.ReadingSecs != 3723 || it.Summary != "A short subtitle" || it.ImageURL != "https://cdn.example.com/show.jpg" || it.Lang != "en-nz" {
		t.Fatalf("item = %+v", it)
	}
	if !strings.Contains(it.ContentHTML, "Long summary text") {
		t.Fatalf("content = %q", it.ContentHTML)
	}
}

func TestYouTubeEntry(t *testing.T) {
	body := `<feed xmlns:yt="http://www.youtube.com/xml/schemas/2015" xmlns:media="http://search.yahoo.com/mrss/" xmlns="http://www.w3.org/2005/Atom">
<title>Channel</title><link rel="alternate" href="https://www.youtube.com/channel/UCabc"/>
<entry><id>yt:video:abc</id><yt:videoId>abc</yt:videoId><title>Video</title>
<link rel="alternate" href="https://www.youtube.com/watch?v=abc"/><published>2023-11-01T00:00:00+00:00</published>
<media:group><media:title>Video</media:title><media:thumbnail url="https://i.ytimg.com/vi/abc/hqdefault.jpg" width="480" height="360"/>
<media:description>Line one
Line two &amp; more</media:description></media:group></entry></feed>`
	items, meta := mustParse(t, []byte(body), "")
	it := items[0]
	if it.Kind != model.KindVideo || meta.Kind != model.KindVideo {
		t.Fatalf("kind = %s", it.Kind)
	}
	if it.ImageURL != "https://i.ytimg.com/vi/abc/hqdefault.jpg" {
		t.Fatalf("image = %q", it.ImageURL)
	}
	if !strings.Contains(it.ContentHTML, "Line one<br/>Line two &amp; more") && !strings.Contains(it.ContentHTML, "Line one<br>Line two &amp; more") {
		t.Fatalf("content = %q", it.ContentHTML)
	}
	if meta.SiteURL != "https://www.youtube.com/channel/UCabc" {
		t.Fatalf("site = %q", meta.SiteURL)
	}
}

func TestEnrichmentAndRelativeURLs(t *testing.T) {
	body := `<rss version="2.0"><channel><title>Blog</title><link>https://blog.example.org/</link>
<item><title></title><link>/posts/hello?utm_source=rss&amp;id=7</link>
<description><![CDATA[<p>Short teaser</p>]]></description>
<content:encoded xmlns:content="http://purl.org/rss/1.0/modules/content/"><![CDATA[<p>Hello <a href="../about">world</a>. <img data-src="img/pic.png" src="data:image/gif;base64,R0lGOD"></p><script>alert(1)</script>` + strings.Repeat("<p>word word word word word word word word word word</p>", 46) + `]]></content:encoded>
<pubDate>Wed, 01 Nov 2023 10:00:00 GMT</pubDate></item>
<item><title>From the future</title><link>https://blog.example.org/f</link><pubDate>Wed, 01 Nov 2099 10:00:00 GMT</pubDate></item>
</channel></rss>`
	items, meta := mustParse(t, []byte(body), "")
	it := items[0]
	if it.URL != "https://blog.example.org/posts/hello?id=7" {
		t.Fatalf("url = %q", it.URL)
	}
	if !strings.Contains(it.ContentHTML, `href="https://blog.example.org/about"`) {
		t.Fatalf("relative link not resolved: %q", it.ContentHTML[:200])
	}
	if it.ImageURL != "https://blog.example.org/posts/img/pic.png" {
		t.Fatalf("lazy image = %q", it.ImageURL)
	}
	if strings.Contains(it.ContentHTML, "<script") {
		t.Fatal("script survived sanitisation")
	}
	if it.Title != "Hello world." {
		t.Fatalf("title fallback = %q", it.Title)
	}
	if it.Summary != "Short teaser" {
		t.Fatalf("summary = %q", it.Summary)
	}
	if it.ReadingSecs < 110 || it.ReadingSecs > 130 { // ~462 words at 230 wpm
		t.Fatalf("reading = %d", it.ReadingSecs)
	}
	if items[1].PublishedAt != now {
		t.Fatalf("future date not clamped: %d", items[1].PublishedAt)
	}
	if meta.SiteURL != "https://blog.example.org/" || meta.Title != "Blog" {
		t.Fatalf("meta = %+v", meta)
	}
}

func TestStableGUIDWithoutIDOrLink(t *testing.T) {
	body := `<rss version="2.0"><channel><title>t</title><item><title>No id</title><description>body</description></item></channel></rss>`
	a, _ := mustParse(t, []byte(body), "")
	b, _ := mustParse(t, []byte(body), "")
	if a[0].GUID != b[0].GUID || a[0].GUID == "" {
		t.Fatal("guid not stable")
	}
}

func TestFindHub(t *testing.T) {
	h := http.Header{}
	h.Add("Link", `<https://pubsubhubbub.appspot.com/>; rel="hub", <https://example.com/feed>; rel="self"`)
	hub, self := findHub(nil, h)
	if hub != "https://pubsubhubbub.appspot.com/" || self != "https://example.com/feed" {
		t.Fatalf("header: %q %q", hub, self)
	}
	body := []byte(`<feed xmlns="http://www.w3.org/2005/Atom"><link rel="hub" href="https://hub.example/"/><link rel="self" href="https://ex.example/atom"/></feed>`)
	hub, self = findHub(body, http.Header{})
	if hub != "https://hub.example/" || self != "https://ex.example/atom" {
		t.Fatalf("body: %q %q", hub, self)
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]int64{"3600": 3600, "59:59": 3599, "1:02:03": 3723, "1:02:03.5": 3723, "": 0, "x": 0} {
		if got := parseDuration(in); got != want {
			t.Errorf("%q = %d, want %d", in, got, want)
		}
	}
}

func TestValidSignature(t *testing.T) {
	sign := func(algo func() hash.Hash, prefix, secret, body string) string {
		m := hmac.New(algo, []byte(secret))
		m.Write([]byte(body))
		return prefix + "=" + hex.EncodeToString(m.Sum(nil))
	}
	if !validSignature(sign(sha256.New, "sha256", "s3cret", "body"), "", "s3cret", []byte("body")) {
		t.Fatal("valid sha256 rejected")
	}
	if !validSignature("", sign(sha1.New, "sha1", "s3cret", "body"), "s3cret", []byte("body")) {
		t.Fatal("valid sha1 rejected")
	}
	if validSignature(sign(sha256.New, "sha256", "wrong", "body"), "", "s3cret", []byte("body")) {
		t.Fatal("wrong secret accepted")
	}
	if validSignature("", "", "s3cret", []byte("body")) {
		t.Fatal("missing signature accepted")
	}
}
