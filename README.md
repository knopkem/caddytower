# CaddyTower

A tiny, RAM-friendly control plane for a single Docker VPS. CaddyTower turns
shared Caddy + Docker + Cloudflare into a guided UI so you can ship GitHub
projects on small instances (2 GB RAM is the design target) without hand-editing
config files.

It is intentionally simpler than SwiftWave or CapRover: no app store, no
in-host builds, no buildpack runtime. You bring GHCR images (or import a repo
and let CaddyTower scaffold the workflow), CaddyTower handles routing,
deploys, domains, DNS, env, databases, logs, health, and rollback.

## Highlights

- One Go binary, server-rendered UI, htmx + custom CSS — no Node runtime needed
- First-run bootstrap with password + TOTP login
- Guided onboarding wizard from fresh install → first live app
- **Import from GitHub**: pick a repo, auto-detect Dockerfile and port,
  optionally open a PR adding the deploy workflow
- Per-project pages with deploy history, **rollback**, env editor, custom
  domains, live logs, runtime stats, optional **health checks + auto-rollback**
- Shared Postgres and MariaDB attachments with per-project credentials
- VPS RAM / disk monitoring with optional email warnings
- Optional daily backups (SQLite + shared DB dumps)
- Audit log of admin actions

## Quickstart

You need a VPS with **Docker (with the Compose plugin)**. Nothing else.

```bash
# 1. Pull the repo onto the VPS (or just the deploy/ + scripts/ directories).
git clone https://github.com/knopkem/caddytower /opt/caddytower-src
cd /opt/caddytower-src
```

```bash
# 2. Run the bootstrap script. It checks Docker, creates the `edge` network,
#    copies the compose + Caddy files, generates a master key, and starts
#    shared-caddy + watchtower if they are not already present.
./scripts/bootstrap-caddytower.sh /opt/caddytower
```

```bash
# 3. The first run stops after writing /opt/caddytower/caddytower.env so you
#    can fill in CADDYTOWER_IMAGE, CADDYTOWER_PUBLIC_BASE_URL, and
#    CADDYTOWER_ROOT_DOMAIN. Edit it, then run the script again.
./scripts/bootstrap-caddytower.sh /opt/caddytower
```

The controller binds to `127.0.0.1:8080` only, so the first login goes through
an SSH tunnel:

```bash
ssh -L 8080:127.0.0.1:8080 ubuntu@your-vps
```
```bash
open http://127.0.0.1:8080/setup
```

Create the first admin user, scan the TOTP QR with any authenticator
(1Password, Bitwarden, Aegis, Google/Microsoft Authenticator, Authy, …), and
sign in. The dashboard opens **Guided start** automatically.

## Getting your first project live

After login, the dashboard walks you through four steps:

1. **Welcome** — what CaddyTower does.
2. **Domain** — set the root domain and origin hostname (or IP) in Settings.
   Cloudflare points your subdomains at this origin.
3. **GitHub** — if you configured a GitHub App, install it and connect.
4. **Deploy** — either:
   - **Import from GitHub** → pick a repo. CaddyTower detects the root
     `Dockerfile`, the first `EXPOSE` port, and an existing image-publishing
     workflow. If no workflow exists, it can open a PR that adds
     `.github/workflows/caddytower-deploy.yml`. The generated workflow builds
     to GHCR and pings `/api/webhooks/deploy/{slug}` after each push.
   - **Manual project** → enter any image such as
     `ghcr.io/owner/app:latest`. Subdomain, internal port, and env are
     captured inline.

From there, each project gets its own page with redeploy, env editor, custom
domains, deploy history, rollback, live logs, runtime stats, and (optional)
health checks. Destructive actions use confirmation dialogs, and common
errors render with next-step hints.

## Two paths after setup

| If you want… | Use |
| --- | --- |
| One-click deploy from an existing GitHub repo | **Import from GitHub** |
| Deploy an image you already publish elsewhere | **Manual project** |
| Manage an already-running container on this VPS | **Adopt** (Settings) |

Repositories that depend on `docker-compose.yml` are intentionally pushed
back to the manual flow — CaddyTower imports single-image GHCR projects, not
compose stacks.

## Essential environment variables

The bootstrap script writes a `caddytower.env` file. The values you must set
before going public:

| Variable | Purpose |
| --- | --- |
| `CADDYTOWER_IMAGE` | GHCR image of CaddyTower itself |
| `CADDYTOWER_PUBLIC_BASE_URL` | Final HTTPS admin URL once Caddy fronts it |
| `CADDYTOWER_ROOT_DOMAIN` | Root domain that hosts your subdomains |
| `CADDYTOWER_MASTER_KEY` | 32-byte base64 key (auto-generated on first run); required to encrypt secrets at rest |

To unlock **Import from GitHub**, also set all four of:

```
CADDYTOWER_GITHUB_APP_ID
CADDYTOWER_GITHUB_APP_SLUG
CADDYTOWER_GITHUB_APP_PRIVATE_KEY_PATH
CADDYTOWER_GITHUB_WEBHOOK_SECRET
```

Configure the GitHub App with:

- **Install URL:** `https://<github>/apps/<slug>/installations/new`
- **Webhook URL:** `https://<your-host>/api/webhooks/github`
- **Webhook secret:** same value as `CADDYTOWER_GITHUB_WEBHOOK_SECRET`
- **Repo permissions:** Metadata (read), Contents (read+write), Pull requests (read+write)
- **Events:** `installation`, `installation_repositories`

Mount the App's private key into the container and point
`CADDYTOWER_GITHUB_APP_PRIVATE_KEY_PATH` at it.

Backups, SMTP warnings, GHES base URLs, and the full table of tunables are
documented in [`deploy/caddytower.env.example`](deploy/caddytower.env.example).

## Local development

```bash
go run ./cmd/caddytower
# then open http://localhost:8080
```

Tests:

```bash
CGO_ENABLED=0 go test ./...
```

## Security notes

- Never expose the controller's port directly. Front it with Caddy and keep it
  on the loopback or the Docker `edge` network.
- The Docker socket mount gives CaddyTower full control of the host's Docker.
  Only run it for trusted admins.
- Webhook calls must include `X-Signature: sha256=<hex HMAC>` of the raw body
  with the per-project secret.
- TOTP secrets, project env, DB credentials, and Cloudflare tokens are
  encrypted at rest with `CADDYTOWER_MASTER_KEY`.
- `X-Forwarded-For` is only trusted from loopback and private peers, so
  clients cannot spoof rate-limit identities.

## More docs

- [`docs/MIGRATION.md`](docs/MIGRATION.md) — cutover runbook for moving an
  existing hand-written Caddy setup onto CaddyTower without downtime.
- [`deploy/caddytower.env.example`](deploy/caddytower.env.example) — every
  available environment variable with defaults.

## HTTP surface (for reference)

- `/` dashboard · `/setup` first admin · `/login` password + TOTP
- `/settings` GitHub status, VPS health, backups, adoption, audit log
- `/projects/import` GitHub import wizard · `/github/install` install redirect
- `/projects/{id}` operations page
- `/projects/{id}/logs/stream` and `/events/stream` SSE
- `/api/webhooks/deploy/{slug}` signed redeploy webhook
- `/api/webhooks/github` signed GitHub App webhook
- `/healthz` · `/readyz` · `/-/version`
