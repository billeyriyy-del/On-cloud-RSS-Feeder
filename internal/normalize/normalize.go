package normalize

import (
	"html"
	"net/url"
	"regexp"
	"strings"

	"github.com/microcosm-cc/bluemonday"
)

var (
	// trackingRe matches query-parameter keys that are tracking noise.
	trackingRe = regexp.MustCompile(`(?i)^(utm_|fbclid$|gclid$|msclkid$|yclid$|mc_eid$|ref$)`)

	ugcPolicy = bluemonday.UGCPolicy()

	tagRe  = regexp.MustCompile(`<[^>]+>`)
	spaceRe = regexp.MustCompile(`\s+`)
)

// StripTrackingParams removes well-known tracking query parameters from rawURL.
// Returns rawURL unchanged on parse error.
func StripTrackingParams(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
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

// SanitizeHTML runs rawHTML through bluemonday's UGC policy.
func SanitizeHTML(rawHTML string) string {
	return ugcPolicy.Sanitize(rawHTML)
}

// HTMLToText converts sanitized HTML to plain text suitable for FTS indexing.
func HTMLToText(sanitized string) string {
	text := tagRe.ReplaceAllString(sanitized, " ")
	text = html.UnescapeString(text)
	return strings.TrimSpace(spaceRe.ReplaceAllString(text, " "))
}
