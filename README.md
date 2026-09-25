# Noema server

A self-hosted, single-user feed reader backend in Go: RSS, Atom, JSON Feed,
podcasts, YouTube and plain web pages go in. A sync API for the SwiftUI apps, a
built-in web app and a small voice agent come out. It is built for one person
with roughly 300 feeds on a 1 GB VM.

## Run

```sh
export RSS_JWT_SECRET=$(openssl rand -hex 32)
export RSS_PASSWORD='choose-something-long'
go run ./cmd/rssfeeder
# open http://localhost:8080
```

| Variable | Default | Meaning |
|---|---|---|
| `RSS_JWT_SECRET` | (required) | HMAC key for tokens. Use 32 or more random characters. |
| `RSS_PASSWORD` | (required) | The single user's password. |
| `RSS_DB_PATH` | `./rssfeeder.db` | SQLite file. WAL mode, so keep the `-wal`/`-shm` files next to it. |
| `RSS_PORT` | `8080` | HTTP port. Put TLS in front, e.g. Caddy. |
| `RSS_RETENTION_DAYS` | `90` | Hard cutoff for unstarred items. Saved items are kept forever. |
| `RSS_PUBLIC_URL` | (empty) | Public base URL, e.g. `https://noema.example.com`. Turns on WebSub push. |
| `RSS_TZ` | server local | IANA zone used for “today” in voice queries, e.g. `Pacific/Auckland`. |
| `RSS_BACKUP_DIR` | (empty) | If set, writes a consistent `VACUUM INTO` snapshot daily and keeps 7. |
| `RSS_WEB_UI` | `1` | Set `0` to serve the API only. |

Operational endpoints: `GET /healthz` (no auth) and `GET /debug/vars`
(expvar counters under `noema`: polls, failures, items ingested, pushes,
full-text results, repaired feeds; auth required).

## What it handles

- **Formats:** RSS 0.9x/1.0/2.0, Atom 0.3/1.0 and JSON Feed 1.0/1.1.
  Podcasts get enclosures, duration, artwork and subtitles. YouTube and Media
  RSS get thumbnails and descriptions. Sites without a feed are followed by
  scraping, with readability extraction.
- **Broken feeds:**
  - BOMs, wrong or missing charsets, and Windows-1252 bytes inside "UTF-8".
  - Control characters, bare `&`, HTML-only entities, and junk before or after
    the XML.
  - Relative links and lazy-loaded images, tracking parameters, future dates,
    and missing titles or GUIDs.
- **Misbehaving servers:**
  - Conditional GET on every poll, with a 5 MB response cap.
  - A 301/308 redirect moves the subscription. 410 retires it, 404 backs off,
    and 429/503 honour `Retry-After`.
  - A feed URL that now returns a web page self-heals through
    autodiscovery.
- **Finding feeds:** paste almost anything, such as a site, a YouTube channel,
  a subreddit, a GitHub repo, Medium, Substack, Mastodon, Bluesky or an Apple
  Podcasts page. `POST /discover` or a URL-only `POST /sources` finds the
  feed.
- **Full text:** opt in per source, or fetch on demand, for teaser-only feeds.
- **Push:** when `RSS_PUBLIC_URL` is set, feeds that advertise a WebSub hub
  deliver in seconds. Polling drops to a 6-hour safety net.

## Clients

- **Web app** at `/`: an installable PWA that works offline. It supports:
  - Gestures: swipe right to mark read, swipe left to save, press and hold
    for a preview, pull down to refresh.
  - Keyboard shortcuts; press `?` in the app for the list.
  - VoiceOver and Dynamic Type.
  - Voice commands through the microphone button.
- **Native apps** (SwiftUI + GRDB) talk to the same API. See
  [docs/API.md](docs/API.md), especially *Sync* and *Notes for native
  clients*.

## Development

```sh
go test ./...
```

The storage tests cover:
- exact sync paging when every write shares one second
- delete propagation
- compaction resets
- migration from a pre-migration database
- per-field last-write-wins

The fetcher tests run real HTTP against broken feeds, redirects, rate limits,
WebSub and readability extraction.

Schema changes go in `internal/storage/schema.go` as a new entry in
`migrations`. Never edit one that has shipped.
