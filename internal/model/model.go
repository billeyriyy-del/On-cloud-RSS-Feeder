package model

// SourceType distinguishes RSS/Atom feeds from HTML-scraped blogs.
type SourceType string

const (
	SourceTypeRSS  SourceType = "rss"
	SourceTypeHTML SourceType = "html"
)

// Source represents a single feed or scrapeable blog.
type Source struct {
	ID            int64          `json:"id"`
	Type          SourceType     `json:"type"`
	URL           string         `json:"url"`
	Title         string         `json:"title"`
	FolderID      *int64         `json:"folder_id,omitempty"`
	ScraperConfig *ScraperConfig `json:"scraper_config,omitempty"`
	NextPollAt    int64          `json:"next_poll_at"`
	LastPollAt    *int64         `json:"last_poll_at,omitempty"`
	ETag          string         `json:"-"`
	LastModified  string         `json:"-"`
	ConsecFails   int            `json:"consec_fails"`
	IsDead        bool           `json:"is_dead"`
	PollInterval  int64          `json:"poll_interval"` // seconds
	CreatedAt     int64          `json:"created_at"`
	UpdatedAt     int64          `json:"updated_at"`
}

// ScraperConfig holds per-site extraction rules for HTML sources.
type ScraperConfig struct {
	IndexSelector   string `json:"index_selector"`
	DateSelector    string `json:"date_selector,omitempty"`
	ContentSelector string `json:"content_selector,omitempty"`
}

// Item is the normalized content unit produced by both ingestion paths.
type Item struct {
	ID          int64  `json:"id"`
	SourceID    int64  `json:"source_id"`
	GUID        string `json:"guid"`
	URL         string `json:"url"`
	Title       string `json:"title"`
	Author      string `json:"author"`
	PublishedAt int64  `json:"published_at"`
	FetchedAt   int64  `json:"fetched_at"`
	ContentHTML string `json:"content_html"`
	ContentText string `json:"-"` // FTS index only, not sent to clients
	CreatedAt   int64  `json:"created_at"`
}

// ItemState tracks read/star state separately to avoid contending with content writes.
type ItemState struct {
	ItemID    int64  `json:"item_id"`
	IsRead    bool   `json:"is_read"`
	IsStarred bool   `json:"is_starred"`
	ReadAt    *int64 `json:"read_at,omitempty"`
	StarredAt *int64 `json:"starred_at,omitempty"`
	UpdatedAt int64  `json:"updated_at"`
}

// Folder represents a named group of sources with optional nesting (max 3 levels).
type Folder struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	ParentID  *int64 `json:"parent_id,omitempty"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// Tombstone records deletions for delta sync.
type Tombstone struct {
	EntityType string `json:"entity_type"`
	EntityID   int64  `json:"entity_id"`
	DeletedAt  int64  `json:"deleted_at"`
}

// SyncDelta is the response payload for GET /sync.
type SyncDelta struct {
	Items      []*Item      `json:"items"`
	States     []*ItemState `json:"states"`
	Folders    []*Folder    `json:"folders"`
	Sources    []*Source    `json:"sources"`
	Tombstones []*Tombstone `json:"tombstones"`
	Cursor     int64        `json:"cursor"`
}

// PollState is written back to the sources table after each poll attempt.
type PollState struct {
	LastPollAt   int64
	NextPollAt   int64
	PollInterval int64
	ETag         string
	LastModified string
	ConsecFails  int
	IsDead       bool
}
