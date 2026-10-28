# fēdan — design

A self-hosted daemon that pulls RSS/Atom feeds and pushes new items to
destinations. Minimal and lightweight: one static Go binary, SQLite, an htmx UI
embedded with `go:embed`.

## Routing

- Destinations are [Shoutrrr](https://github.com/nicholas-fedor/shoutrrr) URLs
  (the maintained `nicholas-fedor` fork), linked into the binary.
- Routing is **by tag only**: feeds carry tags, destinations subscribe to tags.
  There are no direct feed → destination links.
- An item reaches each destination at most once, even when several of its
  feed's tags lead there. An article (same link) already delivered to a
  destination from another feed is not delivered again.
- Every new (or imported) feed gets the `main` tag. It is removable; a feed with
  no tags is still fetched and stored, and the UI warns that it routes nowhere.
- Tags are free-form lowercase slugs, created on first use. Any destination may
  subscribe to `main`.

### Default destination

- Created on first start from `FEDAN_DEFAULT_URL`; startup fails if the DB has
  no default destination and the variable is unset.
- After that the env var is ignored (a warning is logged if it differs).
- It is subscribed to `main`. It can be edited but not deleted, and its `main`
  subscription cannot be removed.
- It receives operational alerts (failing destinations, moved/gone feeds).

## Configuration

Feeds, tags, destinations and subscriptions live in the database and are
managed from the UI. Environment variables cover process-level settings only:

| Variable | Default | Purpose |
|---|---|---|
| `FEDAN_ADDR` | `:8080` | listen address |
| `FEDAN_DB_PATH` | `fedan.db` (`/data/fedan.db` in the image) | SQLite file |
| `FEDAN_DEFAULT_URL` | — | default destination, first start only |
| `FEDAN_PASSWORD_HASH` / `FEDAN_PASSWORD` | — | UI password (bcrypt hash preferred) |
| `FEDAN_INTERVAL` | `15m` | polling interval for all feeds |
| `FEDAN_MAX_ITEMS_PER_FETCH` | `10` | cap on new items per feed per cycle |
| `FEDAN_FETCH_CONCURRENCY` | `4` | parallel fetches |
| `FEDAN_RETENTION` | `168h` | item/delivery history retention |
| `FEDAN_LOG_FORMAT` | `text` | `text` or `json` |

## Fetching

- One global interval with jitter; no per-feed or adaptive intervals.
- Conditional GET (`ETag` / `Last-Modified`), honour `Cache-Control: max-age`
  and `Retry-After`/429, exponential backoff on repeated errors (interval
  doubling, capped at 6h; reset on success).
- 30s timeout, 10 MB body cap, `User-Agent: fedan/<version> (+https://github.com/mdallastella/fedan)`,
  up to 5 redirects. No private-address blocking (LAN feeds are legitimate).
- 301/308: the stored feed URL is rewritten. 410, or 404 for more than 7 days:
  the feed is disabled. Both are logged, shown in the UI and sent to the
  default destination.
- Parsing via `gofeed` (RSS, Atom, JSON Feed). Adding a feed by page URL
  auto-discovers it through `<link rel="alternate">`.

### New items

- Item key: first of GUID/Atom `id`, link, `sha256(title|published)`; unique
  per feed. Edited items are not resent.
- When a feed is added, its current items are marked seen and nothing is sent.
  The UI offers "send latest item" to test routing.
- At most `FEDAN_MAX_ITEMS_PER_FETCH` items per feed per cycle are queued; the
  rest are marked seen and logged.

## Delivery

- The fetcher only writes an outbox (`delivery` rows: item × destination,
  status `pending` / `sent` / `failed`, attempts, last error). A separate
  worker sends.
- Retries with exponential backoff (1m doubling, capped at 1h) for up to 24h,
  then `failed`. Failed deliveries can be retried from the UI.
- When a destination has been failing for 5 minutes, and later when it
  recovers, one alert goes to the default destination (shorter glitches alert
  neither). A template error fails the delivery at once without retries and
  does not count against the destination. Failures of the default destination itself are only
  logged and shown in the UI.

### Message format

- Each destination has a Go `text/template` for title and body, edited in the
  UI with a preview against a real item. Default: title `{{.Item.Title}}`,
  body `{{.Feed.Title}}\n{{.Item.Link}}`.
- Each destination has an output format (`text` / `markdown` / `html`); feed
  HTML fields are converted to it, and the message is truncated to the
  destination's max length.

## Storage

- SQLite via `modernc.org/sqlite` (pure Go, `CGO_ENABLED=0`), plain
  `database/sql`, embedded `migrations/*.sql` tracked with `user_version`.
- Item history (title, link, published, summary, deliveries) is pruned after
  `FEDAN_RETENTION`. Seen keys are pruned only once the item is no longer in
  the feed, so it can't reappear as new.

## UI

- Dashboard, feeds (list, add, edit tags, fetch now, send latest), feed detail
  (recent items and deliveries), destinations (URL, tags, template, format,
  max length, send test, preview), deliveries (failed/pending, retry),
  settings (OPML import/export, effective config).
- Tags are chips with autocomplete, not a separate page.
- `html/template`, vendored htmx and Pico.css (classless), no JS build step.
- OPML import/export maps categories/folders ↔ tags.

## Security

- Single user. Password from env; session cookie (Secure, HttpOnly,
  SameSite=Strict) and CSRF protection via `http.CrossOriginProtection`.
- Destination URLs are masked in the UI and write-only on edit.
- Without a password configured, fēdan only binds to localhost.

## Operations

- `slog` logs to stderr; unauthenticated `/healthz` (204 healthy, 503 if the DB
  or scheduler is down). No metrics endpoint.
- Distribution: GitHub releases (binary) and a `linux/amd64` distroless/static,
  non-root image on GHCR with data in `/data`, plus a compose example.
- Module `github.com/mdallastella/fedan`, MIT licence.
