# Migration runbook

Use this sequence to move a live VPS from a handwritten shared Caddy setup to
CaddyTower-managed routes without dropping existing subdomains.

## 1. Prepare the controller

- Build and push the current CaddyTower image to GHCR.
- On the VPS, install Docker with the Compose plugin.
- Copy these files to the host:
  - `deploy/docker-compose.caddytower.yml`
  - `deploy/caddytower.env.example`
  - `deploy/Caddyfile`
  - `scripts/bootstrap-caddytower.sh`
- Set at least:
  - `CADDYTOWER_IMAGE`
  - HTTPS `CADDYTOWER_PUBLIC_BASE_URL`
  - `CADDYTOWER_ROOT_DOMAIN`

## 2. Boot CaddyTower beside the current stack

- Run:

  ```bash
  ./scripts/bootstrap-caddytower.sh /opt/caddytower
  ```

- Confirm the `caddytower` container joins the `edge` network.
- If `shared-caddy` or `watchtower` are missing, the bootstrap script starts the
  bundled versions automatically.
- If you already have a live shared Caddy stack, leave it untouched for now.

## 3. Tunnel in and finish first-time setup

- Open an SSH tunnel to `127.0.0.1:8080`.
- Complete `/setup`.
- Sign in and save deployment settings in **Settings**.
- Enter the same root domain and origin hostname currently used by the existing
  shared Caddy setup.

## 4. Import the current workload

- Use **Adopt existing containers** from **Settings**.
- Verify that imported projects match the running containers, images, and ports.
- Open each adopted project and confirm:
  - subdomains
  - published ports
  - env values
  - webhook secrets

## 5. Take a safety snapshot

- If backups are enabled, click **Run backup now**.
- Download the generated archive locally before changing route ownership.
- Keep a copy of the current `Caddyfile.shared` as a rollback artifact.

## 6. Hand route ownership to the Admin API

- Ensure the imported web projects cover every hostname CaddyTower should own.
- Trigger a redeploy for one low-risk adopted web project first.
- Verify that the route still resolves correctly.
- Once confirmed, let CaddyTower reconcile the rest of the managed hosts.

Because CaddyTower merges managed hosts into the existing JSON config, unmanaged
routes stay in place.

## 7. Clean up the old manual path

- After verifying adopted hosts work through CaddyTower, stop editing
  `Caddyfile.shared`.
- Remove legacy per-app Caddyfile fragments only after their matching projects
  are visible and healthy in the dashboard.
- Keep the old file around as `.bak` until you have at least one successful
  nightly backup.

## 8. Roll back if needed

- If a migrated hostname breaks, stop the affected CaddyTower project from the
  dashboard or container layer.
- Restore the previous Caddyfile-based route from the saved backup copy.
- Redeploy the original app container if the imported settings drifted from the
  live configuration.
