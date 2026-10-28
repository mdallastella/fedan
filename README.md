# fēdan

A minimal, self-hosted daemon that periodically pulls RSS/Atom feeds and pushes
new items to one or more destinations (Telegram, ntfy, Matrix, email, webhooks, …).

- Feeds carry **tags**; destinations subscribe to tags. New feeds get the `main`
  tag, which routes to the default destination.
- Destinations are delivered through [Shoutrrr](https://github.com/nicholas-fedor/shoutrrr).
- Single static Go binary, SQLite storage, htmx UI embedded with `go:embed`.

See [docs/design.md](docs/design.md) for the design decisions.

## Development

```sh
make dev     # run on :8080
make build   # static binary ./fedan
make test
make lint
```

## License

MIT
