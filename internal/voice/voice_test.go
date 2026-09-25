package voice

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rssfeeder/internal/model"
	"rssfeeder/internal/storage"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want Command
	}{
		{"What's new?", Command{Intent: Brief}},
		{"Hey Noema, catch me up", Command{Intent: Brief}},
		{"what did I miss in Tech today", Command{Intent: Brief, Scope: "tech", Period: "today"}},
		{"anything new on hacker news this week", Command{Intent: Brief, Scope: "hacker news", Period: "week"}},
		{"read the second one", Command{Intent: Read, Ordinal: 2}},
		{"read it", Command{Intent: Read}},
		{"read number 4", Command{Intent: Read, Ordinal: 4}},
		{"open the last one", Command{Intent: Open, Ordinal: -1}},
		{"next", Command{Intent: Next}},
		{"go back", Command{Intent: Previous}},
		{"save it", Command{Intent: Star}},
		{"bookmark this", Command{Intent: Star}},
		{"remove star", Command{Intent: Unstar}},
		{"mark it as read", Command{Intent: MarkRead}},
		{"mark all as read in design", Command{Intent: MarkAll, Scope: "design"}},
		{"keep it unread", Command{Intent: MarkUnread}},
		{"how many unread", Command{Intent: Count}},
		{"what's it about", Command{Intent: Summary}},
		{"search for rust async", Command{Intent: Search, Query: "rust async"}},
		{"anything about climate policy today", Command{Intent: Search, Query: "climate policy", Period: "today"}},
		{"news about apple", Command{Intent: Search, Query: "apple"}},
		{"my account settings page", Command{Intent: Search, Query: "my account settings page"}}, // not "count"
		{"stop", Command{Intent: Stop}},
		{"help", Command{Intent: Help}},
		{"banana", Command{Intent: Help}},
		{"有什么新的", Command{Intent: Brief}},
		{"读第二条", Command{Intent: Read, Ordinal: 2}},
		{"收藏", Command{Intent: Star}},
		{"全部已读", Command{Intent: MarkAll}},
		{"搜索 机器人", Command{Intent: Search, Query: "机器人"}},
	}
	for _, c := range cases {
		if got := Parse(c.in); got != c.want {
			t.Errorf("Parse(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestConversation(t *testing.T) {
	s, err := storage.New(filepath.Join(t.TempDir(), "v.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	tech := &model.Folder{Name: "Tech"}
	s.CreateFolder(ctx, tech)
	src := &model.Source{Type: model.SourceTypeRSS, URL: "https://t.example/feed", Title: "The Verge", FolderID: &tech.ID}
	s.CreateSource(ctx, src)
	other := &model.Source{Type: model.SourceTypeRSS, URL: "https://o.example/feed", Title: "Cooking"}
	s.CreateSource(ctx, other)
	loc, _ := time.LoadLocation("Pacific/Auckland")
	now := time.Date(2026, 9, 25, 20, 0, 0, 0, loc)
	add := func(srcID int64, guid, title string, pub time.Time) {
		s.UpsertItem(ctx, &model.Item{SourceID: srcID, GUID: guid, URL: "https://x.example/" + guid, Title: title,
			Summary: "About " + title, ContentHTML: "<p>First paragraph of " + title + ".</p><p>Second paragraph.</p>",
			ContentText: title, PublishedAt: pub.Unix()})
	}
	add(src.ID, "a", "Chips get faster", now.Add(-2*time.Hour))
	add(src.ID, "b", "Phones get bigger", now.Add(-26*time.Hour)) // yesterday
	add(other.ID, "c", "Better bread", now.Add(-time.Hour))

	a := &Agent{Store: s, Loc: loc, Now: func() time.Time { return now }}
	turn := func(text string, c *Context) *Response {
		t.Helper()
		r, err := a.Handle(ctx, Request{Text: text, Context: c})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	r := turn("what did I miss in tech today", nil)
	if len(r.Items) != 1 || r.Items[0].Title != "Chips get faster" || !strings.Contains(r.Speak[0], "One new story in Tech today") {
		t.Fatalf("scoped brief: %+v %q", r.Items, r.Speak)
	}
	r = turn("what's new", nil)
	if len(r.Items) != 3 || r.Items[0].Title != "Better bread" {
		t.Fatalf("brief: %d items", len(r.Items))
	}
	c := r.Context
	r = turn("read the second one", &c)
	if r.Item.Title != "Chips get faster" || len(r.Speak) < 2 || !strings.Contains(strings.Join(r.Speak, " "), "Second paragraph.") {
		t.Fatalf("read: %+v %q", r.Item, r.Speak)
	}
	if st, _ := s.GetItemState(ctx, r.Item.ID); !st.IsRead {
		t.Fatal("reading aloud should mark read")
	}
	c = r.Context
	r = turn("next", &c)
	if r.Item.Title != "Phones get bigger" || r.Context.Pos != 2 {
		t.Fatalf("next: %+v pos=%d", r.Item, r.Context.Pos)
	}
	c = r.Context
	r = turn("save it", &c)
	if st, _ := s.GetItemState(ctx, r.Item.ID); !st.IsStarred || r.Speak[0] != "Saved." {
		t.Fatalf("save: %+v", r.Speak)
	}
	c = r.Context
	if r = turn("next", &c); r.Speak[0] != "That's the end of the list." {
		t.Fatalf("end: %q", r.Speak)
	}
	c = r.Context
	if r = turn("open it", &c); r.Action == nil || r.Action.Type != "open" || r.Action.URL != "https://x.example/b" {
		t.Fatalf("open: %+v", r.Action)
	}
	one := Context{IDs: []int64{r.Item.ID}, Pos: -1}
	if r = turn("read the third one", &one); !strings.Contains(r.Speak[0], "only has 1") {
		t.Fatalf("out of range: %q", r.Speak)
	}
	if r = turn("how many unread", nil); r.Speak[0] != "2 unread stories." {
		t.Fatalf("count: %q", r.Speak)
	}
	if r = turn("mark all as read", nil); len(r.UndoIDs) != 2 {
		t.Fatalf("mark all: %+v", r.UndoIDs)
	}
	if r = turn("what's new in gardening", nil); !strings.Contains(r.Speak[0], "couldn't find") {
		t.Fatalf("unknown scope: %q", r.Speak)
	}
	if r = turn("what's new", nil); r.Speak[0] != "You're all caught up." {
		t.Fatalf("caught up: %q", r.Speak)
	}
	r, _ = a.Handle(ctx, Request{Text: "有什么新的", Lang: "zh-CN"})
	if r.Speak[0] != "都读完了。" {
		t.Fatalf("zh: %q", r.Speak)
	}
}
