# Noema API

All endpoints use and return JSON, and every timestamp is in unix seconds.
Errors look like `{"error": "…"}`.

## Auth

`POST /auth/login` with `{"password": "…"}` returns
`{"token": "…", "expires_at": …}`.

- Native clients send `Authorization: Bearer <token>`. Tokens last 90 days
  and there is no refresh flow: sign in again when a request returns 401.
- With `{"password": "…", "cookie": true}`, the server sets an HttpOnly
  SameSite=Strict session cookie instead. This is for the web app. A request
  authenticated by that cookie must also send `X-Noema-Client: <anything>`
  on every non-GET call.
- After 8 failed logins from one IP, or 40 in total, within 15 minutes, login
  returns `429` with `Retry-After`. Existing tokens keep working.

Other auth endpoints:

- `POST /auth/logout` clears the cookie.
- `GET /auth/check` returns 200 if you are signed in.

## Sync (native clients)

```
GET /sync?cursor=<n>&limit=<1..1000, default 500>
→ { items, states, folders, sources, tombstones, cursor, has_more, reset }
```

The algorithm:

1. Start with `cursor = 0`.
2. Apply every row in the page with an upsert by id.
3. Apply `tombstones` (`{entity_type: item|source|folder, entity_id}`) as
   deletes. Deleting an item also deletes its state.
4. Store `cursor` in the same local transaction as step 2, **after**
   applying.
5. If `has_more` is true, request the next page immediately.
6. If `reset` is true, your cursor is older than deletions the server has
   compacted away. Wipe the local store and restart from `cursor = 0`. This
   only happens after being offline for longer than the retention window.

Guarantees:

- The cursor is a change-log sequence, not a time. Pages never skip or
  repeat because of equal timestamps, bulk imports or long offline gaps.
- Each row arrives in its *current* state. Several edits to one row inside a
  page collapse into one row.
- A row may arrive again later. Applying a row is idempotent, so that is
  harmless.

The legacy `?since=<unix seconds>` form still works. It maps to a cursor that
cannot skip anything changed at or after that second.

Item fields:

- `id`, `source_id`, `guid`, `url`, `title`, `author`, `summary` (plain text,
  ≤ 280 characters), `image_url`
- `kind`: `article`, `audio` or `video`
- `lang`, `reading_secs` (reading time for articles, duration for media)
- `published_at`, `fetched_at`, `updated_at`
- `content_html`: sanitised, with absolute URLs. When extracted full text
  exists, it replaces the feed's content here.
- `has_full_text`
- `enclosures`: `[{url, type, length, duration}]`

State fields are `item_id`, `is_read`, `is_starred`, `read_at`,
`starred_at`, `read_changed_at`, `star_changed_at` and `updated_at`.

## State changes

```
POST /items/state   {"ops": [{"id": 12, "is_read": true, "at": 1727400000}, …]}   (≤ 1000 ops)
→ {"states": [...]}
```

- Each flag is **last-write-wins on `at`**, the client's clock at the
  moment of the gesture. Record `at` when the user acts, not when the queue
  flushes.
- An offline replay from one device cannot undo a newer change from another
  device.
- An `at` more than 5 minutes in the future is clamped to server time.
- Ops for items that no longer exist are skipped.
- The legacy `PUT /items/{id}/state` accepts the same fields for one item.

```
POST /items/mark-read  {"source_id"?, "folder_id"?, "max_id"?, "before"?, "ids"?, "read"?: true, "at"?}
→ {"ids": [...], "count": n}
```

- `folder_id` includes subfolders.
- Pass `max_id` (the newest item id the user saw) so items that arrive
  meanwhile stay unread.
- `before` limits the change to items published earlier, as in "mark older
  as read".
- To **undo**, send the returned `ids` with `"read": false`.

## Timeline and items (web or thin clients)

```
GET /items?filter=all|unread|starred&source_id=&folder_id=&kind=&since=&max_id=
          &order=newest|oldest&q=&content=1&limit=1..200&cursor=
→ {"items": [ItemView…], "next_cursor": "…"}
```

- `ItemView` is an item plus `is_read`, `is_starred` and `source_title`.
- Content is omitted unless `content=1`.
- Pass `next_cursor` back as `cursor`. It is empty on the last page.
- With `q` (full-text search, prefix match on the last word, safe for any
  input) results are ranked. The cursor then acts as an offset.

Other item endpoints:

- `GET /items/{id}` returns `{"item": ItemView}`.
  - `?full=1` extracts the full article now if it was never tried. Add
    `&retry=1` to try again after a failure.
  - `?original=1` returns the feed's own content.
  - If extraction fails, the response adds `full_text_error`.
- `GET /items/{id}/speech` returns `{title, lang, chunks: [..]}`, with
  utterances of 600 characters or fewer for text-to-speech.
- `GET /items/search?q=&limit=` is the original search endpoint, kept for
  compatibility.
- `GET /counts` returns
  `{unread, starred, by_source: {id: n}, max_id}`, for badges and sidebars.

## Sources and folders

- `POST /discover {"url": "…"}` returns `{"candidates": [{url, title, type,
  kind, via, items}]}`, best first.
  - `via` is `direct`, `rule` (known sites), `link` (`<link rel=alternate>`),
    `guess` (e.g. `/feed`) or `scrape` (no feed; follow the page by
    scraping).
- `POST /sources` subscribes. The body is `{url, type?, title?, folder_id?,
  fetch_full_text?, poll_interval?, scraper_config?}`.
  - Without `type`, the URL goes through discovery and the best feed is
    subscribed. The response is `201 {"source", "alternatives"}`.
  - If only a web page was found, the server returns `422` with
    `candidates`. Re-post with `"type": "html"` to scrape it.
  - `409` means you are already subscribed.
  - New sources are polled immediately.
- `PUT /sources/{id}` is a partial update of `title`, `url`, `folder_id`,
  `fetch_full_text`, `poll_interval` (900–86400), `scraper_config` and
  `is_dead`.
  - `"folder_id": null` moves the source to the top level.
  - `"is_dead": false` revives a retired feed.
- `DELETE /sources/{id}` deletes the source. It also unsubscribes from
  WebSub.
- `POST /sources/refresh {"source_id"?}` queues a poll now (pull-to-refresh)
  and returns `202 {"queued": n}`. Sources polled in the last 2 minutes are
  skipped.
- Source objects also carry `site_url`, `icon_url`, `description`, `kind`
  (the feed's dominant kind), `last_error`, `consec_fails`, `is_dead` and
  `push` (WebSub active).
- Folders:
  - `GET /folders` lists them.
  - `POST /folders {name, parent_id?}` creates one. The limit is 3 levels.
  - `PUT /folders/{id} {name?, parent_id?}` renames or moves one. `null`
    moves it to the top level, and a folder cannot move into its own
    subtree.
  - `DELETE /folders/{id}` deletes one. Its feeds move up to the top level.
- OPML:
  - `POST /opml/import` takes the raw XML body. Nested folders are kept, and
    re-importing is safe.
  - `GET /opml/export` returns the subscriptions.

## Voice agent

```
POST /ask {"text": "what did I miss in tech today", "lang": "en-NZ", "context": <previous response's context>}
→ {command, text, speak: [utterances…], items?, item?, action?, undo_ids?, context}
```

Speech recognition and synthesis stay on the device: `SFSpeechRecognizer` and
`AVSpeechSynthesizer` on Apple platforms, the Web Speech API in browsers. The
server parses intent without any model, so it needs no GPU, no third-party
API, and gives the same answer every time. The parser understands English and
basic Chinese; replies are in Chinese when `lang` starts with `zh`.

| Say | Does |
|---|---|
| "what's new", "catch me up", "what did I miss in *Tech* today / this week" | Unread briefing, scoped to a folder or feed and a period (in `RSS_TZ`) |
| "read the second one", "read it" | Reads the article aloud, fetching the full text for teasers, and marks it read |
| "next", "go back", "what's it about" | Moves through the list or gives the summary |
| "open it", "save it", "unsave", "mark it read / unread" | Acts on the current story |
| "mark all as read (in *X*)" | Scoped mark-read. `undo_ids` lets the client undo it |
| "how many unread (in *X*)", "what have I saved" | Counts and saved items |
| "search for *…*", "anything about *…*" (other free text is treated as a search) | Full-text search |
| "stop" | `action: {type: "stop"}` |

`action` values:

- `open` means show the item (`item_id`, `url`).
- `play` means play the media at `url`.
- `stop` means stop speaking.

Always send the returned `context` back with the next request. It holds the
current list and position, so "next" and "save it" keep working. The server
keeps no conversation state.

`GET /briefing?scope=&period=today|yesterday|week&lang=` gives the same
spoken digest as a one-shot request, for widgets and Shortcuts.

## WebSub (push)

When `RSS_PUBLIC_URL` is set:

- Feeds that advertise a hub (a `Link` header or `<link rel="hub">`) get
  subscribed at `/websub/{source_id}`.
- Pushes must be HMAC-signed with the per-subscription secret. Unsigned or
  mis-signed bodies are acknowledged and ignored.
- Leases renew a day before they expire.

## Notes for native clients

- **Gestures:**
  - Swipe right toggles read; swipe left toggles saved. Show undo.
  - Long-press opens a preview with actions.
  - Pull to refresh calls `POST /sources/refresh`, then syncs.
- **Offline queue:** store the ops you send to `/items/state` with their `at`,
  flush in order, and keep them until the server answers 2xx. Last-write-wins
  makes retries safe.
- **VoiceOver:** VoiceOver users cannot perform custom swipes. Expose read,
  save and preview as `accessibilityActions` on each row, and make the row's
  label read as title, source, age, then unread/saved state.
- **Dynamic Type:** use text styles, not fixed sizes. At accessibility sizes
  let row layouts stack vertically, and drop the thumbnail before truncating
  the title.
- **Keyboard (iPad and Mac):** mirror the web shortcuts with
  `.keyboardShortcut`: j/k, o, m, s, v, r, ⇧A, /, and . for voice.
- **Reduced motion:** honour `accessibilityReduceMotion` for swipe springs and
  sheet transitions.
