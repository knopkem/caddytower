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
- Optional backup archives with manual download from the dashboard
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
| `CADDYTOWER_MASTER_KEY` | _(empty)_ | Base64-encoded 32-byte AES-GCM key for encrypted secrets; required for non-local public URLs |
| `CADDYTOWER_BACKUPS_ENABLED` | `false` | Enable scheduled and manual backups |
| `CADDYTOWER_BACKUPS_RETENTION_DAYS` | `14` | Days of backup archives to keep |
| `CADDYTOWER_BACKUPS_SCHEDULE_UTC` | `02:30` | Daily backup time in UTC (`HH:MM`) |
| `CADDYTOWER_BACKUPS_INCLUDE_ENGINE_DUMPS` | `true` | Include shared Postgres/MariaDB dumps in archives |

The container image overrides `CADDYTOWER_DATA_DIR` to `/data`.

For production exposure, `CADDYTOWER_PUBLIC_BASE_URL` must use HTTPS and
`CADDYTOWER_MASTER_KEY` must be set so project environment values, database
credentials, Cloudflare tokens, and TOTP secrets are encrypted at rest.

## Security notes

- Keep the controller bound to localhost or reachable only through Caddy; never
  publish the container port directly to the internet.
- The Docker socket mount gives CaddyTower administrative control over Docker on
  the host. Only run this controller for trusted administrators.
- CaddyTower only trusts `X-Forwarded-For`/`X-Real-IP` when the immediate peer is
  a loopback or private-network proxy, which prevents direct clients from
  spoofing login and webhook rate-limit identities.
- Webhook requests must include `X-Signature: sha256=<hex HMAC>` over the raw
  body using the per-project webhook secret.
- Backups are disabled by default because archives can grow quickly when shared
  database dumps are included.

## Backups

- **Disabled by default**
- When enabled, scheduled automatically every day at the configured UTC time
- Stored under `${CADDYTOWER_DATA_DIR}/backups`
- Each archive includes:
  - `state.db` from SQLite
  - optional `postgres.sql` when the shared Postgres engine exists
  - optional `mariadb.sql` when the shared MariaDB engine exists
  - `metadata.json` with the snapshot timestamp and trigger
- Old archives are pruned after the configured retention period
- The dashboard shows whether backups are enabled and supports **Run backup now** only when enabled

## Migration runbook

Use this sequence to move the live VPS from the handwritten shared Caddy setup to CaddyTower-managed routes without dropping existing subdomains.

1. **Prepare the controller**
    - Build/push the current image to GHCR.
    - Copy `deploy/docker-compose.caddytower.yml`, `deploy/caddytower.env.example`, and `scripts/bootstrap-caddytower.sh` to the VPS.
    - Set `CADDYTOWER_IMAGE`, HTTPS `CADDYTOWER_PUBLIC_BASE_URL`, and `CADDYTOWER_ROOT_DOMAIN` in `caddytower.env`.

2. **Boot CaddyTower beside the current stack**
   - Run `./scripts/bootstrap-caddytower.sh /opt/caddytower`.
   - Confirm the `caddytower` container joins the existing `edge` network.
   - Keep the current `shared-caddy` container and `Caddyfile.shared` untouched at this stage.

3. **Tunnel in and finish first-time setup**
   - Open an SSH tunnel to `127.0.0.1:8080`.
   - Complete `/setup`, then save deployment settings in the dashboard.
   - Enter the same root domain and origin hostname currently used by the shared Caddy setup.

4. **Import the current workload**
   - Use **Adopt existing containers** from the dashboard.
   - Verify imported projects match the running containers, images, and ports.
   - Open each adopted project and confirm subdomains, published ports, env vars, and webhook secrets look sane.

5. **Take a safety snapshot**
   - If backups are enabled, use **Run backup now** in the dashboard.
   - Download the generated archive locally before changing Caddy ownership.
   - Keep a copy of the current `Caddyfile.shared` as an extra rollback artifact.

6. **Hand route ownership to the Admin API**
   - Ensure the imported web projects cover every hostname you want CaddyTower to own.
   - Trigger a redeploy for one low-risk adopted web project first and verify the route still resolves correctly.
   - Once confirmed, let CaddyTower reconcile the rest of the managed hosts.
   - Because managed routes are merged into the current JSON config, unmanaged Caddy routes remain in place.

7. **Clean up the old manual path**
   - After verifying all adopted hosts work through CaddyTower, stop editing `Caddyfile.shared`.
   - Remove legacy per-app Caddyfile fragments only after their matching projects are visible and healthy in the dashboard.
   - Keep the old file around as `.bak` until you have at least one successful nightly backup.

8. **Rollback if needed**
   - If a migrated hostname breaks, stop the affected CaddyTower project from the dashboard or container layer.
   - Restore the previous Caddyfile-based route from the saved backup copy.
   - Redeploy the original app container if the imported settings drifted from the live configuration.

## Endpoints

- `/` — scaffold landing page
- `/setup` — first-admin bootstrap
- `/login` — password + TOTP login
- `/backups/{name}` — authenticated backup download
- `/projects/{projectID}/logs/stream` — authenticated live log stream (SSE)
- `/api/webhooks/deploy/{slug}` — signed redeploy webhook
- `/healthz` — liveness probe
- `/readyz` — readiness probe
- `/-/version` — build metadata as JSON
