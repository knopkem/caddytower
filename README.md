# CaddyTower

A tiny, RAM-friendly control plane for a single Docker VPS. CaddyTower turns
shared Caddy + Docker, with optional Cloudflare DNS automation, into a guided UI so you can ship GitHub
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
# Install from the latest tagged release (recommended).
curl -fsSL https://raw.githubusercontent.com/knopkem/caddytower/main/install.sh | bash
```

The installer defaults to:

- `ghcr.io/knopkem/caddytower:latest`
- `/opt/caddytower`
- a local SSH-tunnel bootstrap first, unless you choose the final public HTTPS URL
- GitHub App setup **later in the app**, unless you explicitly opt into the
  advanced installer-based GitHub flow

It walks you through the important values instead of requiring a repo checkout
or manual file edits up front.

```bash
# Install from main only if you explicitly want the moving branch.
curl -fsSL https://raw.githubusercontent.com/knopkem/caddytower/main/install.sh | bash -s -- --ref main
```

The installer resolves the latest release tag by default, downloads the install
assets for that tag, writes `/opt/caddytower`, generates
`CADDYTOWER_MASTER_KEY`, and starts bundled `shared-caddy` / `watchtower` only
when they are missing, including existing compose-managed containers with
generated names.

If the target directory or Docker access needs elevated privileges, the script
will request `sudo` when needed instead of failing immediately.

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

If you want the old local-repo workflow for development or manual tweaking, you
can still clone the repo and run:

```bash
./scripts/bootstrap-caddytower.sh /opt/caddytower
```

## Getting your first project live

After login, the dashboard walks you through four steps:

1. **Welcome** — what CaddyTower does.
2. **Domain** — set the root domain and origin hostname (or IP) in Settings.
   Manual DNS works with any provider. If you use Cloudflare and want automatic
   DNS updates, add the optional zone ID and API token there too. CaddyTower
   uses `https://caddytower.<root-domain>` as the default admin hostname and
   manages the shared Caddy route for it.
3. **GitHub** — when you want repo imports, follow the GitHub setup guide in
   Settings after the public admin hostname is reachable, then connect the
   GitHub App.
4. **Controller updates** — Settings checks the latest tagged release and lets
   you trigger an on-demand controller refresh or release update with one click.
   CaddyTower pulls the target image and replaces its own container
   automatically.
5. **Deploy** — either:
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

To unlock **Import from GitHub**, finish the normal first login, make sure the
public admin URL is reachable, then open **Settings → GitHub import** and save:

- the numeric GitHub App ID
- the GitHub App slug
- the webhook secret
- the downloaded private key PEM

Configure the GitHub App in GitHub with:

- **Install URL:** `https://<github>/apps/<slug>/installations/new`
- **Webhook URL:** `https://<your-host>/api/webhooks/github`
- **Webhook secret:** the same value you paste into CaddyTower
- **Repo permissions:** Metadata (read), Contents (read+write), Pull requests (read+write)
- **Events:** `installation`, `installation_repositories`

CaddyTower stores the webhook secret and private key encrypted at rest, so the
normal setup flow no longer needs an env-file edit or a manual PEM mount. The
installer still supports an explicit advanced path via `--enable-github` for
legacy or highly customized installs.

For a step-by-step explanation of these values, the Settings page fields,
Cloudflare setup, the GitHub App flow, backups, SMTP alerts, and common setup
mistakes, see [`docs/SETUP.md`](docs/SETUP.md).

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

- [`docs/SETUP.md`](docs/SETUP.md) — full install and configuration tutorial,
  including GitHub App setup and optional features.
- [`docs/MIGRATION.md`](docs/MIGRATION.md) — cutover runbook for moving an
  existing hand-written Caddy setup onto CaddyTower without downtime.
- [`install.sh`](install.sh) — curl-first guided installer entrypoint.
- [`deploy/caddytower.env.example`](deploy/caddytower.env.example) — every
  available environment variable with defaults.

## HTTP surface (for reference)

- `/` dashboard · `/setup` first admin · `/login` password + TOTP
- `/settings` GitHub status, VPS health, backups, audit log
- `/projects/import` GitHub import wizard · `/github/install` install redirect
- `/projects/{id}` operations page
- `/projects/{id}/logs/stream` and `/events/stream` SSE
- `/api/webhooks/deploy/{slug}` signed redeploy webhook
- `/api/webhooks/github` signed GitHub App webhook
- `/healthz` · `/readyz` · `/-/version`
