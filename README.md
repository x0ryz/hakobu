# hakobu — self-hosted deployment tool

Deploy apps from GitHub to your own server. Push to the default branch and
hakobu rebuilds and rolls out the new version with zero downtime.

- **Projects** group apps, PostgreSQL databases, S3 storages and shared variables.
- **Apps** come from GitHub repos, built with a Dockerfile or [Railpack](https://railpack.com) (auto-detected).
- **Zero-downtime deploys** with blue/green containers behind an in-process proxy, plus one-click rollback.
- **Volumes**: directories that survive redeploys; an app with volumes is restarted on deploy (a few seconds of downtime) unless you let both versions share them.
- **Databases** live in one shared Postgres container, each with its own role; optional daily backups to a storage.
- **Storages**: self-hosted RustFS on the same server, Cloudflare R2 or any S3-compatible bucket.
- **Variables**: shared per project and per service; linked databases/storages inject `DATABASE_URL`, `POSTGRES_*`, `S3_*`.
- **Logs**: build/deploy logs, container output, and errors via an auto-injected `SENTRY_DSN`.
- **Sign-in with GitHub only**; the owner can allow more GitHub users.
- **Cloudflare Tunnel**: panel and apps on your domain with HTTPS, no open ports.
- **Keeps the disk in check**: each app keeps only its live image and one for rollback; unused build cache is dropped daily.
- Single Go binary + SQLite. Needs only Docker.

## Install

You need a Linux server and a domain on Cloudflare (the free plan is enough).
The server needs no public IP or open ports: everything goes through a Cloudflare Tunnel.

```bash
curl -fsSL https://raw.githubusercontent.com/x0ryz/hakobu/main/install.sh | sudo bash
```

1. The installer shows a Cloudflare link: sign in and select **Authorize**. Hakobu gets
   permission to read your domains, manage their DNS records and create a tunnel.
   Nothing to copy: the terminal continues on its own.
2. Pick one of your domains from the list and the panel's subdomain (default `hakobu`).
   Hakobu creates the tunnel and a DNS record for the panel.
3. Open the printed link, `https://hakobu.example.com/setup?token=…`, click
   **Connect GitHub** (this registers a private GitHub App for your panel), sign in with
   GitHub and you are the owner. Only that link can claim a fresh panel.

Every app then gets its own address in any domain of the account (`app.example.com`,
`myapp.dev`, …); hakobu creates and removes the DNS records itself.

Lost the setup link: `journalctl -u hakobu | grep setup`. Sign in to Cloudflare again:
`cd /opt/hakobu && sudo ./hakobu setup --reconnect`.

### OAuth relay

Cloudflare OAuth clients have one redirect URL while every hakobu server has its own
address, so the login goes through a tiny Worker (`relay/`): it keeps the authorization
code for up to five minutes until the installer that started the login fetches it.
The code is useless without the PKCE verifier that stays on the server.

## Using it

1. Create a project.
2. **New app** → pick a repo (give the GitHub App access to it first), check the detected stack. The name and the `<app>.<your domain>` address are filled in; pick another domain from the list or "private". The first deploy starts right away.
3. Add databases and storages on the project page, then link them on the app's Overview tab.
4. Put variables used by all apps under **Shared variables**, app-specific ones on the app's **Variables** tab. Changes apply on the next deploy.
5. Pushes to the repo's default branch redeploy automatically; **Redeploy** rebuilds the latest commit, **Rollback** returns to the previous build.

The app gets `PORT` to listen on; hakobu detects the port it actually listens on either way.

## Local development

```bash
go build -o hakobu . && ./hakobu agent --public-host <host that reaches 127.0.0.1:9000 over https>
# e.g. cloudflared tunnel --url http://127.0.0.1:9000 for a throwaway host
```

Requires Go 1.27+ and Docker (plus Railpack and a `buildkit` container for non-Dockerfile builds).
State lives in `data/` (SQLite, certificates, clones).

### Database changes

- Schema: `internal/store/migrations/NNN_name.sql`, applied in order at startup and tracked in `PRAGMA user_version`. Add a new file for every change, never edit one that has shipped.
- Queries: `internal/store/queries.sql`, compiled by [sqlc](https://sqlc.dev) into `internal/store/*.gen.go`. After changing either, run:

```bash
go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest
sqlc generate
```

## Layout

- `cmd/` — CLI, web panel (`web.go` + `web.html`), GitHub webhook, Sentry ingest
- `internal/ops/` — projects, apps, deploys, databases, storages, backups
- `internal/deploy/` — Docker Engine API client
- `internal/proxy/` — per-app reverse proxy for blue/green swaps
- `internal/edge/` — routes tunnel traffic by host to the panel or an app
- `internal/cloudflare/`, `internal/tunnel/` — Cloudflare OAuth + API, the cloudflared process
- `relay/` — Cloudflare Worker for the OAuth callback
- `internal/build/`, `internal/detect/` — cloning and building repos
- `internal/github/` — GitHub App, OAuth
- `internal/store/` — SQLite: migrations, sqlc queries

## License

[MIT](LICENSE)
