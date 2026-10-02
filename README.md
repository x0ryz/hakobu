# hakobu — self-hosted deployment tool

Deploy apps from GitHub to your own server. Push to the default branch and
hakobu rebuilds and rolls out the new version with zero downtime.

- **Projects** group apps, PostgreSQL databases, S3 storages and shared variables. Each project has its own
  Docker networks: an app can reach its project's apps, databases and storages, never another project's apps.
- **Apps** come from GitHub repos, built with a Dockerfile or [Railpack](https://railpack.com) (auto-detected).
- **Zero-downtime deploys** with blue/green containers behind an in-process proxy, plus one-click rollback.
  Before each deploy the app's database is snapshotted on the server, so a rollback can also undo a bad migration.
- **Volumes**: directories that survive redeploys; an app with volumes is restarted on deploy (a few seconds of downtime) unless you let both versions share them.
- **Resource limits**: memory and CPU caps per app and worker; out-of-memory kills show up on the app page.
- **Databases** live in one shared Postgres container, each with its own role. Backups are one click in
  Settings: hakobu creates a private R2 bucket in your Cloudflare account and backs up every database daily.
  No S3 keys exist for it (hakobu writes with its Cloudflare token), and the bucket's lock keeps each backup
  for 7 days even from a bug in hakobu. It doesn't stop someone who has taken over the server: the token
  can change the lock. Each backup's SHA-256 is kept and checked before a restore; each is test-restored
  into a temporary database right after upload, and old ones are rotated (the last 7, one a week for a month).
- **Storages** for your apps' files: self-hosted RustFS on the same server, Cloudflare R2 or any S3-compatible bucket.
- **Variables**: shared per project and per service; linked databases/storages inject `DATABASE_URL`, `POSTGRES_*`, `S3_*`.
- **Logs**: build/deploy logs, container output, and errors via an auto-injected `SENTRY_DSN`.
- **Sign-in with GitHub only**, for the owner: the GitHub account that claimed the panel with the setup link.
- **Cloudflare Tunnel**: panel and apps on your domain with HTTPS, no open ports. cloudflared runs in its own
  container and sends app traffic straight to the app's container, so restarting or upgrading hakobu doesn't take apps down.
- **Secrets encrypted at rest**: variables, database passwords, storage keys and tokens, but also build logs, worker
  commands and your apps' errors and logs, are encrypted in the SQLite database with a key in `key/master.key`, kept
  apart from `data/`; the database snapshots kept for Rollback are sealed with it too. Keep a copy of the key
  wherever you keep a copy of `data/`: without it nothing secret can be read, and hakobu refuses to start rather than make a new key over data it can't read.
- **Keeps the disk in check**: each app keeps only its live image and one for rollback; unused build cache is dropped daily.
- Single Go binary + SQLite. Needs only Docker, which the installer sets up rootless.

## Install

You need a Linux server and a domain on Cloudflare (the free plan is enough).
The server needs no public IP or open ports: everything goes through a Cloudflare Tunnel.

```bash
curl -fsSL https://hakobu.dev/install.sh | sudo bash
```

1. The installer shows a Cloudflare link to a new API token with hakobu's permissions
   filled in: read your domains, manage their DNS records, create a tunnel, keep
   database backups in R2 and email you when something needs you. Select **Continue to summary** → **Create Token**, copy the
   token and paste it into the terminal (it isn't echoed). Or set `CLOUDFLARE_API_TOKEN`
   before running the installer. Under **Zone Resources** you can pick just the domains
   hakobu should use instead of all of them: whoever takes over the server gets the
   token, and with it the DNS of every domain it covers.
2. Pick one of your domains from the list and the panel's subdomain (default `hakobu`).
   Hakobu creates the tunnel and a DNS record for the panel.
3. Open the printed link, `https://hakobu.example.com/setup?token=…`, click
   **Connect GitHub** (this registers a private GitHub App for your panel), sign in with
   GitHub and you are the owner. Only that link can claim a fresh panel.

Every app then gets its own address in any domain of the account (`app.example.com`,
`myapp.dev`, …); hakobu creates and removes the DNS records itself.

Lost the setup link: `journalctl -u hakobu | grep setup`. Give hakobu a new Cloudflare
token (after rolling it, say): `cd /opt/hakobu && sudo -u hakobu ./hakobu setup --reconnect`.

### Email notifications

**Settings → Notifications**: enter your address and confirm it from the email
Cloudflare sends. Hakobu then emails you when a deploy after a push fails, a backup
fails, an app runs out of memory, the disk is almost full or the master key isn't
downloaded; each problem once, again only if it's still there hours later, and once
more when it's gone. It goes through Cloudflare Email Service, free for a confirmed
address, from `alerts@<panel address>` (the name before the @ can be changed). Cloudflare sends from any address of a domain
with Email Routing on; where it's off, hakobu turns it on through
`mail.<panel address>`, which leaves the domain's own mail alone (turning it on for the
domain itself would replace its MX records). A token made before v0.5 lacks the email permissions:
give hakobu a new one with `setup --reconnect`.

### Bringing a panel back on a new server

With backups on, the panel's own database goes to the backup bucket daily, sealed with the
master key. The key stays off the bucket: download it from **Settings → Security →
Download master key** (it asks you to sign in with GitHub again) and keep the file away
from the server; download it again after **Replace all secrets**. On a new server:

```sh
curl -fsSL https://hakobu.dev/install.sh -o install.sh
sudo HAKOBU_RESTORE=hakobu-master-key-hakobu.example.com.txt bash install.sh
```

It asks for a Cloudflare API token that can read R2 (or takes `CLOUDFLARE_API_TOKEN`),
restores the newest panel backup and starts the panel on its old address, with its tunnel,
GitHub App, projects and owner. Then redeploy the apps and restore each database from its
backup page; files in volumes and RustFS storages stay with the old server. If the old
Cloudflare token was rolled since, run `setup --reconnect` as above.

Hakobu runs as the unprivileged `hakobu` user, and
[rootless Docker](https://docs.docker.com/engine/security/rootless/) as another one,
`hakobu-docker`: neither a break-in into hakobu nor a container escape gets root on the
server, and an escape can't read hakobu's data (its secrets and master key). Servers installed before that
keep running as root under the system Docker (their databases live there) and the installer
only updates them; to move one to rootless Docker, install hakobu on a clean server.

### Why an API token, not "Sign in with Cloudflare"

Cloudflare's OAuth token endpoint sits behind the dashboard's bot protection, which
challenges many server networks (Hetzner, DigitalOcean, VPNs; see
[workers-sdk#11081](https://github.com/cloudflare/workers-sdk/issues/11081)). A server
there can't trade the login code for tokens or refresh them, and doing it elsewhere would
let a third party see your tokens. An API token goes straight from the dashboard to your
server, never needs refreshing and works from any network. You can also limit it to the
server's IP address in the dashboard (Client IP Address Filtering).

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
State lives in `data/` (SQLite, clones) and its master key in `key/`; `run/` holds the panel's socket for cloudflared.

Tests that need Docker (they start their own containers) run with `HAKOBU_DOCKER_TEST=1 go test ./...`.

The panel's scripts, styles and fonts are embedded in the binary (`cmd/static/`), so it loads nothing
from other sites. After changing classes in `cmd/web.html`, rebuild the stylesheet with
`go generate ./cmd` (needs [bun](https://bun.sh); runs Tailwind 3 with `cmd/tailwind.config.js`).

### Database changes

- Schema: `internal/store/migrations/NNN_name.sql`, applied in order at startup and tracked in `PRAGMA user_version`. Add a new file for every change, never edit one that has shipped.
- Queries: `internal/store/queries.sql`, compiled by [sqlc](https://sqlc.dev) into `internal/store/*.gen.go`. After changing either, run:

```bash
go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest
sqlc generate
```

## Layout

- `cmd/` — CLI, web panel (`web.go` + `web.html`, assets in `static/`), GitHub webhook, Sentry ingest
- `internal/ops/` — projects, apps, deploys, databases, storages, backups
- `internal/deploy/` — Docker Engine API client
- `internal/proxy/` — per-app reverse proxy on `127.0.0.1:<port>` (private apps, fallback route)
- `internal/edge/` — the panel's router: the panel, or an app the tunnel has no route for yet
- `internal/cloudflare/` — Cloudflare API with the owner's token (the tunnel's routes, DNS records, R2 backups)
- `internal/build/`, `internal/detect/` — cloning and building repos
- `internal/github/` — GitHub App, OAuth
- `internal/store/` — SQLite: migrations, sqlc queries
- `internal/secret/` — encryption of secrets in the database
- `internal/backup/` — streaming pg_dump/restore
- `internal/s3/` — creating RustFS buckets

## License

[MIT](LICENSE)
