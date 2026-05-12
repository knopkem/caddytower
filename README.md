# CaddyTower

CaddyTower is a lightweight control plane for a single Docker VPS running shared
Caddy, Cloudflare DNS, and image-only deployments from GHCR.

This repository currently contains:

- Go 1.25 app with a small chi-based HTTP server
- Embedded HTML templates and static assets
- Health and version endpoints
- First-user bootstrap, password login, TOTP, sessions, and CSRF protection
- Deployment settings for shared Caddy and Cloudflare DNS
- Web project CRUD with Docker reconciliation and Caddy route management
- TCP and UDP project support with published host-port mappings
- Shared Postgres and MariaDB attachments with per-project credentials
- HMAC-signed deploy webhooks for GitHub Actions
- Live per-project log streaming in the admin UI
- Existing-container adoption from Docker + current Caddy routes
- Multi-stage distroless Docker image
- GitHub Actions workflow for test/build/push to GHCR

## Local run

```bash
go run ./cmd/caddytower
```

Open <http://localhost:8080>.

## VPS bootstrap

```bash
./scripts/bootstrap-caddytower.sh /opt/caddytower
```

The bootstrap script:

- creates the external Docker `edge` network if needed
- copies the compose file and example env file into `/opt/caddytower`
- generates `CADDYTOWER_MASTER_KEY` on first run
- starts the controller once `CADDYTOWER_IMAGE` has been configured

The initial compose file binds CaddyTower to `127.0.0.1:8080` only, so the
recommended first-time setup is an SSH tunnel:

```bash
ssh -L 8080:127.0.0.1:8080 ubuntu@your-vps
```

Then open <http://127.0.0.1:8080/setup>.

## Environment

| Variable | Default | Purpose |
| --- | --- | --- |
| `CADDYTOWER_HTTP_ADDR` | `:8080` | HTTP listen address |
| `CADDYTOWER_PUBLIC_BASE_URL` | `http://localhost:8080` | Public URL used in links |
| `CADDYTOWER_DATA_DIR` | `./var` | Persistent app data directory |
| `CADDYTOWER_CADDY_ADMIN_URL` | `http://shared-caddy:2019` | Caddy Admin API base URL |
| `CADDYTOWER_ROOT_DOMAIN` | _(empty)_ | Root domain managed by the controller |
| `DOCKER_HOST` | _(docker default)_ | Docker daemon address |
| `CADDYTOWER_MASTER_KEY` | _(empty)_ | Base64-encoded 32-byte AES-GCM key for encrypted secrets |

The container image overrides `CADDYTOWER_DATA_DIR` to `/data`.

## Endpoints

- `/` — scaffold landing page
- `/setup` — first-admin bootstrap
- `/login` — password + TOTP login
- `/projects/{projectID}/logs/stream` — authenticated live log stream (SSE)
- `/api/webhooks/deploy/{slug}` — signed redeploy webhook
- `/healthz` — liveness probe
- `/readyz` — readiness probe
- `/-/version` — build metadata as JSON
