// Package voice is Noema's lightweight voice agent: a deterministic intent
// parser plus a handler that turns intents into store queries and speakable
// replies. Speech recognition and synthesis stay on the client (Apple's
// Speech/AVSpeechSynthesizer, or the Web Speech API), so the server needs no
// model, no GPU and no third-party API — and every answer is predictable.
package voice

import (
	"regexp"
	"strconv"
	"strings"
)

// Intent is what the user asked for.
type Intent string

const (
	Brief      Intent = "brief"       // what's new / catch me up / what did I miss in X today
	Count      Intent = "count"       // how many unread
	Search     Intent = "search"      // find X / anything about X
	Read       Intent = "read"        // read it / read the second one
	Summary    Intent = "summary"     // what's it about / tell me more
	Next       Intent = "next"        // next / skip
	Previous   Intent = "previous"    // previous / go back
	Open       Intent = "open"        // open it
	Star       Intent = "star"        // save it / star this
	Unstar     Intent = "unstar"      // unsave / remove star
	MarkRead   Intent = "mark_read"   // mark it read / done
	MarkAll    Intent = "mark_all"    // mark all as read
	MarkUnread Intent = "mark_unread" // mark it unread / keep it
	Saved      Intent = "saved"       // what have I saved
	Stop       Intent = "stop"        // stop / pause / be quiet
	Help       Intent = "help"
)

// Command is a parsed utterance.
type Command struct {
	Intent  Intent `json:"intent"`
	Ordinal int    `json:"ordinal,omitempty"` // 1-based; -1 = last; 0 = "it"/current
	Query   string `json:"query,omitempty"`   // search terms
	Scope   string `json:"scope,omitempty"`   // "in <folder or source>"
	Period  string `json:"period,omitempty"`  // today | yesterday | week
}

var (
	punctRe  = regexp.MustCompile(`[^\p{L}\p{N}\s'’-]+`)
	spacesRe = regexp.MustCompile(`\s+`)

	searchRe = regexp.MustCompile(`^(?:search(?: for)?|find(?: me)?|look up|look for|anything (?:about|on)|is there anything (?:about|on)|news (?:about|on)|stories (?:about|on))\s+(.+)$`)
	numberRe = regexp.MustCompile(`\b(?:number|no|item|story)\s*(\d{1,2})\b`)
	scopeRe  = regexp.MustCompile(`\b(?:in|from|on)\s+(?:the\s+|my\s+)?(.+?)(?:\s+(?:today|yesterday|this week|lately|recently))?$`)
)

var ordinals = map[string]int{
	"first": 1, "1st": 1, "one": 1, "second": 2, "2nd": 2, "two": 2, "third": 3, "3rd": 3, "three": 3,
	"fourth": 4, "4th": 4, "four": 4, "fifth": 5, "5th": 5, "five": 5, "sixth": 6, "six": 6,
	"seventh": 7, "seven": 7, "eighth": 8, "eight": 8, "ninth": 9, "nine": 9, "tenth": 10, "ten": 10,
	"last": -1, "latest": 1, "top": 1,
	"第一": 1, "第二": 2, "第三": 3, "第四": 4, "第五": 5, "最后": -1,
}

// Parse maps free text (as produced by speech recognition) to a Command.
// Unrecognised text of two or more words becomes a search; the rest is Help.
func Parse(text string) Command {
	t := normalise(text)
	if t == "" {
		return Command{Intent: Help}
	}
	cmd := Command{Ordinal: ordinal(t), Period: period(t)}
	padded := " " + t + " "
	// has matches whole words/phrases for Latin text ("count" must not match
	// "account") and substrings for CJK, which has no spaces.
	has := func(subs ...string) bool {
		for _, s := range subs {
			if isCJKText(s) {
				if strings.Contains(t, s) {
					return true
				}
			} else if strings.Contains(padded, " "+s+" ") {
				return true
			}
		}
		return false
	}
	starts := func(prefixes ...string) bool {
		for _, p := range prefixes {
			if t == p || strings.HasPrefix(t, p+" ") {
				return true
			}
		}
		return false
	}

	switch {
	case starts("stop", "pause", "quiet", "be quiet", "shut up", "cancel", "never mind", "nevermind") || has("停", "暂停"):
		cmd.Intent = Stop
	case starts("help") || has("what can you do", "what can i say", "帮助"):
		cmd.Intent = Help
	case has("mark all", "mark everything", "clear all", "all read", "全部已读"):
		cmd.Intent = MarkAll
		cmd.Scope = scope(t)
	case has("unread") && has("mark", "keep", "leave") || has("标记未读"):
		cmd.Intent = MarkUnread
	case has("mark", "done with", "i'm done", "im done", "finished") || has("已读"):
		cmd.Intent = MarkRead
	case has("unstar", "unsave", "remove star", "remove from saved", "取消收藏"):
		cmd.Intent = Unstar
	case has("what have i saved", "my saved", "saved stories", "saved items", "starred", "收藏的"):
		cmd.Intent = Saved
	case starts("save", "star", "bookmark", "keep this", "keep that", "keep it", "remember this") || has("收藏"):
		cmd.Intent = Star
	case has("how many", "unread count", "count", "多少"):
		cmd.Intent = Count
		cmd.Scope = scope(t)
	case starts("next", "skip", "next one", "go on", "move on", "下一"):
		cmd.Intent = Next
	case starts("previous", "back", "go back", "last one", "上一"):
		cmd.Intent = Previous
	case starts("open", "show", "show me", "pull up") || has("打开"):
		cmd.Intent = Open
	case has("what's it about", "whats it about", "what is it about", "summary", "summarize", "summarise", "tell me more", "more about", "概要", "摘要"):
		cmd.Intent = Summary
	case starts("read", "read out", "play", "listen to") || has("读"):
		cmd.Intent = Read
	case searchRe.MatchString(t):
		cmd.Intent, cmd.Query = Search, searchRe.FindStringSubmatch(t)[1]
	case has("what's new", "whats new", "what is new", "anything new", "catch me up", "brief", "briefing",
		"what did i miss", "what have i missed", "headlines", "news", "good morning", "new stories", "latest",
		"有什么新", "新闻", "简报"):
		cmd.Intent = Brief
		cmd.Scope = scope(t)
	default:
		if strings.HasPrefix(t, "搜索") {
			cmd.Intent, cmd.Query = Search, strings.TrimSpace(strings.TrimPrefix(t, "搜索"))
		} else if len(strings.Fields(t)) >= 2 || isCJKText(t) {
			cmd.Intent, cmd.Query = Search, t
		} else {
			cmd.Intent = Help
		}
	}
	if cmd.Intent == Search {
		cmd.Query = strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(cmd.Query, " please"), " today"))
	}
	return cmd
}

func normalise(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "’", "'")
	s = punctRe.ReplaceAllString(s, " ")
	s = spacesRe.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	for _, p := range []string{"hey noema ", "noema ", "ok noema ", "okay noema ", "please ", "can you ", "could you ", "would you "} {
		s = strings.TrimPrefix(s, p)
	}
	return strings.TrimSpace(strings.TrimSuffix(s, " please"))
}

func ordinal(t string) int {
	for _, w := range strings.Fields(t) {
		if n, ok := ordinals[w]; ok {
			return n
		}
	}
	for k, n := range ordinals {
		if isCJKText(k) && strings.Contains(t, k) {
			return n
		}
	}
	if m := numberRe.FindStringSubmatch(t); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

func period(t string) string {
	switch {
	case strings.Contains(t, "yesterday") || strings.Contains(t, "昨天"):
		return "yesterday"
	case strings.Contains(t, "today") || strings.Contains(t, "this morning") || strings.Contains(t, "今天"):
		return "today"
	case strings.Contains(t, "this week") || strings.Contains(t, "本周") || strings.Contains(t, "这周"):
		return "week"
	}
	return ""
}

// scope extracts "in <name>" / "from <name>" (e.g. "what's new in tech today").
func scope(t string) string {
	m := scopeRe.FindStringSubmatch(t)
	if m == nil {
		return ""
	}
	s := strings.TrimSpace(m[1])
	for _, drop := range []string{"today", "yesterday", "this week", "stories", "feed", "folder"} {
		s = strings.TrimSpace(strings.TrimSuffix(s, drop))
	}
	return s
}

func isCJKText(s string) bool {
	for _, r := range s {
		if r >= 0x2E80 && r <= 0x9FFF || r >= 0xAC00 && r <= 0xD7AF {
			return true
		}
	}
	return false
}
