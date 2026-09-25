package model

// SourceType distinguishes RSS/Atom/JSON feeds from HTML-scraped blogs.
type SourceType string

const (
	SourceTypeRSS  SourceType = "rss" // any syndication format gofeed understands: RSS 0.9x–2.0, Atom, JSON Feed
	SourceTypeHTML SourceType = "html"
)

// ItemKind tells clients how to present an item.
type ItemKind string

const (
	KindArticle ItemKind = "article"
	KindAudio   ItemKind = "audio" // podcast episode (audio enclosure)
	KindVideo   ItemKind = "video" // YouTube / video enclosure
)

// Source represents a single feed or scrapeable blog.
type Source struct {
	ID            int64          `json:"id"`
	Type          SourceType     `json:"type"`
	URL           string         `json:"url"`
	Title         string         `json:"title"`
	SiteURL       string         `json:"site_url,omitempty"`
	IconURL       string         `json:"icon_url,omitempty"`
	Description   string         `json:"description,omitempty"`
	Kind          ItemKind       `json:"kind"` // dominant kind of the feed's items
	FolderID      *int64         `json:"folder_id,omitempty"`
	ScraperConfig *ScraperConfig `json:"scraper_config,omitempty"`
	FetchFullText bool           `json:"fetch_full_text"`
	NextPollAt    int64          `json:"next_poll_at"`
	LastPollAt    *int64         `json:"last_poll_at,omitempty"`
	ETag          string         `json:"-"`
	LastModified  string         `json:"-"`
	ConsecFails   int            `json:"consec_fails"`
	LastError     string         `json:"last_error,omitempty"`
	IsDead        bool           `json:"is_dead"`
	PollInterval  int64          `json:"poll_interval"` // seconds
	Push          bool           `json:"push"`          // WebSub subscription is active
	WebSub        WebSub         `json:"-"`
	CreatedAt     int64          `json:"created_at"`
	UpdatedAt     int64          `json:"updated_at"`
}

// WebSub holds a source's push subscription (server-side only).
type WebSub struct {
	Hub       string
	Topic     string
	Secret    string
	ExpiresAt int64
}

// ScraperConfig holds per-site extraction rules for HTML sources.
type ScraperConfig struct {
	IndexSelector   string `json:"index_selector"`
	DateSelector    string `json:"date_selector,omitempty"`
	ContentSelector string `json:"content_selector,omitempty"`
}

// Enclosure is an attached media file (podcast audio, video, image).
type Enclosure struct {
	URL      string `json:"url"`
	Type     string `json:"type,omitempty"`     // MIME type
	Length   int64  `json:"length,omitempty"`   // bytes
	Duration int64  `json:"duration,omitempty"` // seconds
}

// Item is the normalized content unit produced by both ingestion paths.
type Item struct {
	ID          int64        `json:"id"`
	SourceID    int64        `json:"source_id"`
	GUID        string       `json:"guid"`
	URL         string       `json:"url"`
	Title       string       `json:"title"`
	Author      string       `json:"author"`
	Summary     string       `json:"summary"`
	ImageURL    string       `json:"image_url,omitempty"`
	Kind        ItemKind     `json:"kind"`
	Lang        string       `json:"lang,omitempty"`
	ReadingSecs int64        `json:"reading_secs"` // estimated reading (or listening/watching) time
	PublishedAt int64        `json:"published_at"`
	FetchedAt   int64        `json:"fetched_at"`
	UpdatedAt   int64        `json:"updated_at"`
	ContentHTML string       `json:"content_html,omitempty"`
	HasFullText bool         `json:"has_full_text"`
	Enclosures  []*Enclosure `json:"enclosures,omitempty"`
	ContentText string       `json:"-"` // FTS index only, not sent to clients
	EntryUpdAt  int64        `json:"-"` // the feed's own <updated>; drives in-place edits
	CreatedAt   int64        `json:"created_at"`
}

// ItemView is an item joined with its state and source title, for timelines.
type ItemView struct {
	*Item
	IsRead      bool   `json:"is_read"`
	IsStarred   bool   `json:"is_starred"`
	SourceTitle string `json:"source_title"`
}

// ItemState tracks read/star state separately to avoid contending with content writes.
//
// Each flag carries the (client-supplied) time it was last changed; writes are
// last-write-wins per field, so offline replays from several devices converge.
type ItemState struct {
	ItemID        int64  `json:"item_id"`
	IsRead        bool   `json:"is_read"`
	IsStarred     bool   `json:"is_starred"`
	ReadAt        *int64 `json:"read_at,omitempty"`
	StarredAt     *int64 `json:"starred_at,omitempty"`
	ReadChangedAt int64  `json:"read_changed_at"`
	StarChangedAt int64  `json:"star_changed_at"`
	UpdatedAt     int64  `json:"updated_at"`
}

// StateOp is one client-side state change (a swipe, a keypress, a voice command).
// Nil fields are left untouched. At is the client's unix time of the change.
type StateOp struct {
	ItemID    int64 `json:"id"`
	IsRead    *bool `json:"is_read,omitempty"`
	IsStarred *bool `json:"is_starred,omitempty"`
	At        int64 `json:"at,omitempty"`
}

// Folder represents a named group of sources with optional nesting (max 3 levels).
type Folder struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	ParentID  *int64 `json:"parent_id,omitempty"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// Tombstone records a deletion for delta sync.
type Tombstone struct {
	EntityType string `json:"entity_type"` // item | source | folder
	EntityID   int64  `json:"entity_id"`
	DeletedAt  int64  `json:"deleted_at"`
}

// SyncDelta is the response payload for GET /sync.
//
// Cursor is an opaque, monotonically increasing change sequence. Clients store it
// and pass it back verbatim; while HasMore is true they should call again at once.
// Reset means the client's cursor predates compacted deletions: drop the local
// cache and start again from cursor 0.
type SyncDelta struct {
	Items      []*Item      `json:"items"`
	States     []*ItemState `json:"states"`
	Folders    []*Folder    `json:"folders"`
	Sources    []*Source    `json:"sources"`
	Tombstones []*Tombstone `json:"tombstones"`
	Cursor     int64        `json:"cursor"`
	HasMore    bool         `json:"has_more"`
	Reset      bool         `json:"reset"`
}

// PollState is written back to the sources table after each poll attempt.
type PollState struct {
	LastPollAt   int64
	NextPollAt   int64
	PollInterval int64
	ETag         string
	LastModified string
	ConsecFails  int
	LastError    string
	IsDead       bool
}

// FeedMeta is feed-level metadata refreshed on successful polls.
type FeedMeta struct {
	Title       string
	SiteURL     string
	IconURL     string
	Description string
	Kind        ItemKind
}
