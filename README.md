# fēdan

A minimal, self-hosted daemon that periodically pulls RSS/Atom feeds and pushes
new items to one or more destinations (Telegram, ntfy, Matrix, email, webhooks, …).

- Feeds carry **tags**; destinations subscribe to tags. New feeds get the `main`
  tag, which routes to the default destination.
- Destinations are delivered through [Shoutrrr](https://github.com/nicholas-fedor/shoutrrr).
- Single static Go binary, SQLite storage, htmx UI embedded with `go:embed`.

See [docs/design.md](docs/design.md) for the design decisions.

## Quick start

```sh
make build
FEDAN_DEFAULT_URL='ntfy://ntfy.sh/your-topic' ./fedan
```

Open http://127.0.0.1:8080, add feeds and destinations. Without a password
fēdan only listens on localhost; set `FEDAN_PASSWORD_HASH` (bcrypt) or
`FEDAN_PASSWORD` to expose it. All settings are listed in
[docs/design.md](docs/design.md#configuration).

## Docker

```sh
docker run --rm -i ghcr.io/mdallastella/fedan hash-password   # type a password
```

Put the hash and your default destination in [compose.yaml](compose.yaml)
(doubling every `$` in the hash), then `docker compose up -d`. Data lives in
the `/data` volume. Images are `linux/amd64`, distroless and non-root;
`latest` is the newest release, `main` the development branch.

## Development

```sh
make dev     # run on :8080
make build   # static binary ./fedan
make test
make lint
```

## License

MIT
