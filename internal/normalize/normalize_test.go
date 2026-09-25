package normalize

import (
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestReadingSecsCountsCJK(t *testing.T) {
	en := strings.Repeat("word ", 230)
	if got := ReadingSecs(en); got != 60 {
		t.Fatalf("230 English words = %ds, want 60", got)
	}
	zh := strings.Repeat("触觉传感器", 80) // 400 characters, no spaces
	if got := ReadingSecs(zh); got != 60 {
		t.Fatalf("400 CJK characters = %ds, want 60", got)
	}
}

func TestSpeechChunks(t *testing.T) {
	long := strings.Repeat("This is a sentence that goes on. ", 40)
	chunks := SpeechChunks("<h2>Heading</h2><p>Short one.</p><p>"+long+"</p><ul><li>a</li><li>b</li></ul>", 300)
	if len(chunks) < 4 || !strings.HasPrefix(chunks[0], "Heading") {
		t.Fatalf("chunks = %q", chunks)
	}
	for _, c := range chunks {
		if utf8.RuneCountInString(c) > 300 {
			t.Fatalf("chunk too long: %d", utf8.RuneCountInString(c))
		}
	}
}

func TestResolveURLsAndAlt(t *testing.T) {
	base, _ := url.Parse("https://ex.example/blog/post")
	out := ResolveURLs(`<a href="../x">x</a><img data-src="i.png" src="data:,"><img src="/j.png" alt="J">`, base)
	for _, want := range []string{`href="https://ex.example/x"`, `src="https://ex.example/blog/i.png"`, `alt=""`, `alt="J"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in %s", want, out)
		}
	}
}

func TestStripTrackingParams(t *testing.T) {
	got := StripTrackingParams("https://ex.example/a?utm_source=x&id=1&fbclid=2")
	if got != "https://ex.example/a?id=1" {
		t.Fatalf("got %s", got)
	}
}
