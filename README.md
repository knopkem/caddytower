# CaddyTower

CaddyTower is a lightweight control plane for a single Docker VPS running shared
Caddy, Cloudflare DNS, and image-only deployments from GHCR.

This repository currently contains the initial scaffold:

- Go 1.22 app with a small chi-based HTTP server
- Embedded HTML templates and static assets
- Health and version endpoints
- Multi-stage distroless Docker image
- GitHub Actions workflow for test/build/push to GHCR

## Local run

```bash
go run ./cmd/caddytower
```

Open <http://localhost:8080>.

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
- `/healthz` — liveness probe
- `/readyz` — readiness probe
- `/-/version` — build metadata as JSON
