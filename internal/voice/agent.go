package voice

import (
	"context"
	"fmt"
	"strings"
	"time"

	"rssfeeder/internal/model"
	"rssfeeder/internal/normalize"
	"rssfeeder/internal/storage"
)

// Context is the conversation state. The server is stateless: the client
// sends back the Context from the previous Response, which is what makes
// follow-ups like "read the second one", "next" and "save it" work.
type Context struct {
	IDs      []int64 `json:"ids,omitempty"` // the list last spoken
	Pos      int     `json:"pos"`           // current item in IDs (-1: none yet)
	SourceID int64   `json:"source_id,omitempty"`
	FolderID int64   `json:"folder_id,omitempty"`
	Since    int64   `json:"since,omitempty"`
	Scope    string  `json:"scope,omitempty"` // display name of the scope
}

// Request is one utterance.
type Request struct {
	Text    string   `json:"text"`
	Lang    string   `json:"lang,omitempty"` // BCP-47 of the recogniser, e.g. "en-NZ", "zh-CN"
	Context *Context `json:"context,omitempty"`
}

// Action is something the client should do besides speaking.
type Action struct {
	Type   string `json:"type"` // open | play | stop
	ItemID int64  `json:"item_id,omitempty"`
	URL    string `json:"url,omitempty"`
}

// Response is what to say and show.
type Response struct {
	Command Command           `json:"command"`
	Text    string            `json:"text"`  // short display text
	Speak   []string          `json:"speak"` // utterances, in order
	Items   []*model.ItemView `json:"items,omitempty"`
	Item    *model.ItemView   `json:"item,omitempty"` // the story in focus
	Action  *Action           `json:"action,omitempty"`
	UndoIDs []int64           `json:"undo_ids,omitempty"` // for "mark all as read"
	Context Context           `json:"context"`
}

// Agent answers voice requests from the store.
type Agent struct {
	Store *storage.Store
	Loc   *time.Location
	Now   func() time.Time
	// FullText, when set, is used to fetch the whole article before reading
	// aloud an item whose feed only carries a teaser.
	FullText func(ctx context.Context, itemID int64, url string) error
}

const briefSize = 5

// Handle runs one turn of the conversation.
func (a *Agent) Handle(ctx context.Context, req Request) (*Response, error) {
	cmd := Parse(req.Text)
	return a.Run(ctx, cmd, req)
}

// Run executes an already-parsed command (used by GET /briefing too).
func (a *Agent) Run(ctx context.Context, cmd Command, req Request) (*Response, error) {
	c := Context{Pos: -1}
	if req.Context != nil {
		c = *req.Context
	}
	p := phrases(req.Lang)
	r := &Response{Command: cmd, Context: c, Speak: []string{}}

	switch cmd.Intent {
	case Brief, Count, MarkAll:
		if err := a.applyScope(ctx, cmd, &r.Context); err != nil {
			return nil, err
		}
		if cmd.Scope != "" && r.Context.Scope == "" {
			r.say(p.f("noscope", cmd.Scope))
			return r, nil
		}
		q := storage.ItemQuery{Unread: true, SourceID: r.Context.SourceID, FolderID: r.Context.FolderID, Since: r.Context.Since, Limit: briefSize}
		switch cmd.Intent {
		case Count:
			n, err := a.Store.CountItems(ctx, q)
			if err != nil {
				return nil, err
			}
			r.say(p.count(n, r.Context.Scope))
		case MarkAll:
			counts, err := a.Store.GetCounts(ctx)
			if err != nil {
				return nil, err
			}
			ids, err := a.Store.MarkRead(ctx, storage.MarkScope{SourceID: r.Context.SourceID, FolderID: r.Context.FolderID,
				Since: r.Context.Since, MaxID: counts.MaxID, Read: true, At: a.now().Unix()})
			if err != nil {
				return nil, err
			}
			r.UndoIDs = ids
			r.say(p.f("marked_all", len(ids)))
		default:
			return r, a.brief(ctx, r, q, p)
		}

	case Saved:
		items, err := a.Store.ListItems(ctx, storage.ItemQuery{Starred: true, Limit: briefSize})
		if err != nil {
			return nil, err
		}
		r.list(items, p, p.f("saved_n", len(items)), p.s("saved_none"))

	case Search:
		items, err := a.Store.ListItems(ctx, storage.ItemQuery{Q: cmd.Query, Limit: briefSize})
		if err != nil {
			return nil, err
		}
		r.list(items, p, p.f("found_n", len(items), cmd.Query), p.f("found_none", cmd.Query))

	case Next, Previous:
		if len(c.IDs) == 0 {
			return r, a.brief(ctx, r, storage.ItemQuery{Unread: true, Limit: briefSize}, p)
		}
		step := 1
		if cmd.Intent == Previous {
			step = -1
		}
		pos := c.Pos + step
		if pos < 0 || pos >= len(c.IDs) {
			r.say(p.s("end_of_list"))
			return r, nil
		}
		it, err := a.Store.GetItem(ctx, c.IDs[pos], false)
		if err != nil || it == nil {
			r.say(p.s("gone"))
			return r, err
		}
		r.Context.Pos = pos
		r.Item = lite(it)
		r.say(p.headline(pos, it), p.s("hint_read"))

	case Read, Summary, Open, Star, Unstar, MarkRead, MarkUnread:
		it, pos, err := a.target(ctx, cmd, &r.Context)
		if err != nil {
			return nil, err
		}
		if it == nil {
			if n := len(r.Context.IDs); n > 0 {
				r.say(p.f("out_of_range", n))
			} else {
				r.say(p.s("nothing_selected"))
			}
			return r, nil
		}
		r.Context.Pos = pos
		return r, a.act(ctx, cmd, it, r, p)

	case Stop:
		r.Action = &Action{Type: "stop"}

	default:
		r.say(p.s("help"))
	}
	return r, nil
}

func (a *Agent) brief(ctx context.Context, r *Response, q storage.ItemQuery, p phrasebook) error {
	total, err := a.Store.CountItems(ctx, q)
	if err != nil {
		return err
	}
	items, err := a.Store.ListItems(ctx, q)
	if err != nil {
		return err
	}
	if total == 0 {
		r.say(p.caughtUp(r.Context.Scope, r.Command.Period))
		return nil
	}
	r.list(items, p, p.newCount(total, r.Context.Scope, r.Command.Period), "")
	return nil
}

// list speaks a short list and makes it the conversation's current list.
func (r *Response) list(items []*model.ItemView, p phrasebook, intro, empty string) {
	if len(items) == 0 {
		r.say(empty)
		return
	}
	r.Items = make([]*model.ItemView, len(items))
	r.Context.IDs = make([]int64, len(items))
	r.say(intro)
	for i, it := range items {
		r.Items[i] = lite(it)
		r.Context.IDs[i] = it.ID
		r.say(p.headline(i, it))
	}
	r.Context.Pos = -1
	r.say(p.s("hint_list"))
}

func (r *Response) say(s ...string) {
	for _, x := range s {
		if x = strings.TrimSpace(x); x != "" {
			r.Speak = append(r.Speak, x)
			if r.Text == "" {
				r.Text = x
			}
		}
	}
}

// target resolves "it", "the second one", "the last one" against the context.
func (a *Agent) target(ctx context.Context, cmd Command, c *Context) (*model.ItemView, int, error) {
	if len(c.IDs) == 0 {
		items, err := a.Store.ListItems(ctx, storage.ItemQuery{Unread: true, Limit: briefSize})
		if err != nil {
			return nil, 0, err
		}
		c.IDs = nil
		for _, it := range items {
			c.IDs = append(c.IDs, it.ID)
		}
		c.Pos = -1
	}
	if len(c.IDs) == 0 {
		return nil, 0, nil
	}
	pos := c.Pos
	switch {
	case cmd.Ordinal > 0:
		pos = cmd.Ordinal - 1
	case cmd.Ordinal == -1:
		pos = len(c.IDs) - 1
	case pos < 0:
		pos = 0
	}
	if pos >= len(c.IDs) {
		return nil, 0, nil
	}
	it, err := a.Store.GetItem(ctx, c.IDs[pos], false)
	return it, pos, err
}

func (a *Agent) act(ctx context.Context, cmd Command, it *model.ItemView, r *Response, p phrasebook) error {
	r.Item = lite(it)
	at := a.now().Unix()
	yes, no := true, false
	switch cmd.Intent {
	case Open:
		r.Action = &Action{Type: "open", ItemID: it.ID, URL: it.URL}
		r.say(p.f("opening", it.Title))
	case Summary:
		sum := it.Summary
		if sum == "" {
			sum = p.s("no_summary")
		}
		r.say(it.Title+".", sum)
	case Star, Unstar:
		v, key := &yes, "saved"
		if cmd.Intent == Unstar {
			v, key = &no, "unsaved"
		}
		if _, err := a.Store.ApplyStateOps(ctx, []model.StateOp{{ItemID: it.ID, IsStarred: v, At: at}}); err != nil {
			return err
		}
		r.say(p.s(key))
	case MarkRead, MarkUnread:
		v, key := &yes, "marked_read"
		if cmd.Intent == MarkUnread {
			v, key = &no, "marked_unread"
		}
		if _, err := a.Store.ApplyStateOps(ctx, []model.StateOp{{ItemID: it.ID, IsRead: v, At: at}}); err != nil {
			return err
		}
		r.say(p.s(key))
	case Read:
		if it.Kind == model.KindAudio || it.Kind == model.KindVideo {
			for _, e := range it.Enclosures {
				if strings.HasPrefix(e.Type, "audio/") || strings.HasPrefix(e.Type, "video/") {
					r.Action = &Action{Type: "play", ItemID: it.ID, URL: e.URL}
					break
				}
			}
			if r.Action == nil {
				r.Action = &Action{Type: "open", ItemID: it.ID, URL: it.URL}
			}
			r.say(p.f("playing", it.Title))
			break
		}
		// A teaser-only feed would be read out in ten seconds: fetch the article.
		if a.wantsFullText(ctx, it) {
			fctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			if a.FullText(fctx, it.ID, it.URL) == nil {
				if fresh, err := a.Store.GetItem(ctx, it.ID, false); err == nil && fresh != nil {
					it = fresh
				}
			}
			cancel()
		}
		intro := it.Title + ". " + p.f("from", it.SourceTitle)
		if it.Author != "" {
			intro += " " + p.f("by", it.Author)
		}
		r.say(intro)
		chunks := normalize.SpeechChunks(it.ContentHTML, 600)
		if len(chunks) > 60 {
			chunks = append(chunks[:60], p.s("truncated"))
		}
		r.say(chunks...)
		r.Text = it.Title
		if _, err := a.Store.ApplyStateOps(ctx, []model.StateOp{{ItemID: it.ID, IsRead: &yes, At: at}}); err != nil {
			return err
		}
	}
	return nil
}

// wantsFullText reports whether an item is a teaser worth expanding before
// reading aloud (and that extraction has not already failed for it).
func (a *Agent) wantsFullText(ctx context.Context, it *model.ItemView) bool {
	if it.HasFullText || a.FullText == nil || it.URL == "" ||
		len([]rune(normalize.HTMLToText(it.ContentHTML))) >= 1000 {
		return false
	}
	st, _ := a.Store.FullTextStatus(ctx, it.ID)
	return st != "failed"
}

// applyScope resolves "in <name>" to a folder or source, and the period to a
// start time in the user's time zone.
func (a *Agent) applyScope(ctx context.Context, cmd Command, c *Context) error {
	if cmd.Period != "" {
		now := a.now().In(a.loc())
		midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		switch cmd.Period {
		case "today":
			c.Since = midnight.Unix()
		case "yesterday":
			c.Since = midnight.AddDate(0, 0, -1).Unix()
		case "week":
			wd := (int(now.Weekday()) + 6) % 7 // Monday = 0
			c.Since = midnight.AddDate(0, 0, -wd).Unix()
		}
	} else if cmd.Scope != "" || cmd.Intent == Brief {
		c.Since = 0
	}
	if cmd.Scope == "" {
		if cmd.Intent == Brief {
			c.SourceID, c.FolderID, c.Scope = 0, 0, ""
		}
		return nil
	}
	c.SourceID, c.FolderID, c.Scope = 0, 0, ""
	want := strings.ToLower(strings.TrimSpace(cmd.Scope))
	folders, err := a.Store.ListFolders(ctx)
	if err != nil {
		return err
	}
	sources, err := a.Store.ListSources(ctx)
	if err != nil {
		return err
	}
	// Exact names beat partial matches; folders beat sources.
	for pass := 0; pass < 2; pass++ {
		match := func(name string) bool {
			n := strings.ToLower(name)
			if pass == 0 {
				return n == want || strings.TrimPrefix(n, "the ") == want
			}
			return strings.Contains(n, want) || (len(n) >= 3 && strings.Contains(want, n))
		}
		for _, f := range folders {
			if match(f.Name) {
				c.FolderID, c.Scope = f.ID, f.Name
				return nil
			}
		}
		for _, s := range sources {
			if match(s.Title) {
				c.SourceID, c.Scope = s.ID, s.Title
				return nil
			}
		}
	}
	return nil
}

func (a *Agent) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Agent) loc() *time.Location {
	if a.Loc != nil {
		return a.Loc
	}
	return time.Local
}

// lite strips content so voice responses stay small.
func lite(v *model.ItemView) *model.ItemView {
	it := *v.Item
	it.ContentHTML = ""
	out := *v
	out.Item = &it
	return &out
}

// ── Phrases ───────────────────────────────────────────────────────────────────

type phrasebook struct {
	lang string
	m    map[string]string
}

func phrases(lang string) phrasebook {
	if strings.HasPrefix(strings.ToLower(lang), "zh") {
		return phrasebook{"zh", zh}
	}
	return phrasebook{"en", en}
}

func (p phrasebook) s(key string) string           { return p.m[key] }
func (p phrasebook) f(key string, a ...any) string { return fmt.Sprintf(p.m[key], a...) }

func (p phrasebook) headline(i int, it *model.ItemView) string {
	ord := ""
	if i >= 0 && i < len(ordWords[p.lang]) {
		ord = ordWords[p.lang][i]
	}
	return fmt.Sprintf(p.m["headline"], ord, it.Title, it.SourceTitle)
}

func (p phrasebook) newCount(n int64, scope, period string) string {
	return p.decorate(p.plural(n, "new_one", "new_n"), scope, period) + p.s("stop")
}

func (p phrasebook) caughtUp(scope, period string) string {
	return p.decorate(p.s("caught_up"), scope, period) + p.s("stop")
}

func (p phrasebook) count(n int64, scope string) string {
	return p.decorate(p.plural(n, "unread_one", "unread_n"), scope, "") + p.s("stop")
}

func (p phrasebook) plural(n int64, one, many string) string {
	if n == 1 {
		return p.s(one)
	}
	return p.f(many, n)
}

func (p phrasebook) decorate(s, scope, period string) string {
	if scope != "" {
		s += p.f("in_scope", scope)
	}
	if period != "" {
		s += p.s("period_" + period)
	}
	return s
}

var ordWords = map[string][]string{
	"en": {"First", "Second", "Third", "Fourth", "Fifth", "Sixth", "Seventh", "Eighth", "Ninth", "Tenth"},
	"zh": {"第一条", "第二条", "第三条", "第四条", "第五条", "第六条", "第七条", "第八条", "第九条", "第十条"},
}

var en = map[string]string{
	"stop":             ".",
	"headline":         "%s: %s, from %s.",
	"new_one":          "One new story",
	"new_n":            "%d new stories",
	"unread_one":       "One unread story",
	"unread_n":         "%d unread stories",
	"caught_up":        "You're all caught up",
	"in_scope":         " in %s",
	"period_today":     " today",
	"period_yesterday": " since yesterday",
	"period_week":      " this week",
	"noscope":          "I couldn't find a folder or feed called %s.",
	"hint_list":        "Say “read the first one”, “next”, or “save it”.",
	"hint_read":        "Say “read it”, “next”, or “open it”.",
	"end_of_list":      "That's the end of the list.",
	"gone":             "That story is no longer available.",
	"nothing_selected": "There's nothing to pick from yet. Try “what's new”.",
	"out_of_range":     "The list only has %d. Say “read the first one”, or “what's new”.",
	"opening":          "Opening %s.",
	"playing":          "Playing %s.",
	"no_summary":       "There's no summary for this one.",
	"saved":            "Saved.",
	"unsaved":          "Removed from saved.",
	"marked_read":      "Marked as read.",
	"marked_unread":    "Marked as unread.",
	"marked_all":       "Marked %d stories as read. Say “undo” in the app to bring them back.",
	"saved_n":          "You have %d saved stories.",
	"saved_none":       "You haven't saved anything yet.",
	"found_n":          "Here's what I found for %[2]s.",
	"found_none":       "Nothing matches %s.",
	"from":             "From %s.",
	"by":               "By %s.",
	"truncated":        "That's as far as I'll read aloud. Open it to read the rest.",
	"help":             "You can say: what's new; what did I miss in a folder today; read the first one; next; open it; save it; mark all as read; or search for a topic.",
}

var zh = map[string]string{
	"stop":             "。",
	"headline":         "%s：%s，来自%s。",
	"new_one":          "有一篇新文章",
	"new_n":            "有%d篇新文章",
	"unread_one":       "有一篇未读",
	"unread_n":         "有%d篇未读",
	"caught_up":        "都读完了",
	"in_scope":         "（%s）",
	"period_today":     "（今天）",
	"period_yesterday": "（昨天以来）",
	"period_week":      "（本周）",
	"noscope":          "没有找到叫%s的文件夹或订阅。",
	"hint_list":        "可以说“读第一条”、“下一条”或“收藏”。",
	"hint_read":        "可以说“读”、“下一条”或“打开”。",
	"end_of_list":      "列表已经到底了。",
	"gone":             "这篇文章已经不在了。",
	"nothing_selected": "还没有可选的文章，试试说“有什么新的”。",
	"out_of_range":     "列表里只有%d篇。",
	"opening":          "正在打开%s。",
	"playing":          "正在播放%s。",
	"no_summary":       "这篇没有摘要。",
	"saved":            "已收藏。",
	"unsaved":          "已取消收藏。",
	"marked_read":      "已标记为已读。",
	"marked_unread":    "已标记为未读。",
	"marked_all":       "已将%d篇标记为已读。",
	"saved_n":          "你收藏了%d篇。",
	"saved_none":       "还没有收藏。",
	"found_n":          "关于%[2]s，找到这些。",
	"found_none":       "没有找到%s。",
	"from":             "来自%s。",
	"by":               "作者%s。",
	"truncated":        "朗读到这里，剩下的请打开阅读。",
	"help":             "你可以说：有什么新的；今天某个文件夹有什么；读第一条；下一条；打开；收藏；全部已读；或者搜索某个话题。",
}
