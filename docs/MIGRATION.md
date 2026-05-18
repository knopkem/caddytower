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

## 4. Recreate the current workload in CaddyTower

- Create each project manually in CaddyTower, or use **GitHub import** for repos
  that already have a clean image-based workflow.
- Copy the real image refs, ports, subdomains, and required environment values
  from the current stack into the new project definitions.
- If the current app depends on host data or mounted config files, copy those
  bind mounts too. CaddyTower now accepts one bind mount per line in:

  ```text
  /host/source | /container/target | rw
  /host/config.json | /app/config.json | ro
  ```

- For web projects that need more than a single host catch-all route, fill in
  **HTTP route rules** using:

  ```text
  @domains | host
  @domains | path_prefix | /api | strip
  example.org | path_exact | /ready | rewrite=/healthz
  ```

  Notes:
  - `@domains` means “the generated subdomain plus any custom domains”.
  - Valid match types are `host`, `path_prefix`, and `path_exact`.
  - Valid transforms are blank, `strip`, or `rewrite=/new-path`.
- Before moving a hostname, open the new project and confirm:
  - subdomains
  - published ports
  - bind mounts
  - HTTP route rules
  - env values
  - webhook secrets

## 5. Take a safety snapshot

- If backups are enabled, click **Run backup now**.
- Download the generated archive locally before changing route ownership.
- Keep a copy of the current `Caddyfile.shared` as a rollback artifact.

## 6. Hand route ownership to the Admin API

- Ensure the recreated web projects cover every hostname CaddyTower should own.
- Trigger a redeploy for one low-risk web project first.
- Verify that the route still resolves correctly.
- Once confirmed, let CaddyTower reconcile the rest of the managed hosts.

Because CaddyTower merges managed hosts into the existing JSON config, unmanaged
routes stay in place. Managed routes are now matched by **host + route matcher**
instead of host only, so one managed `/api` split route no longer causes every
other route on the same host to be replaced.

## 6a. Migration recipes by workload shape

### Single-container site

- Create a normal **web** project.
- Use the current image ref and internal port.
- Leave **Bind mounts** empty unless the app reads a host file or persistent
  data directory.
- Leave **HTTP route rules** empty or keep the default catch-all host route.

### App with persistent host data

- Create a **web** project.
- Copy the current image, subdomain, and port.
- Add the persistent data bind mount, for example:

  ```text
  /srv/app/data | /app/data | rw
  ```

- Start with the default catch-all host route.

### App with a mounted web-server config

- Recreate the public-facing service as a **web** project.
- Mount the config file the container already expects, for example:

  ```text
  /srv/app/nginx.conf | /etc/nginx/conf.d/default.conf | ro
  ```

- Recreate any companion TCP/UDP service as a separate managed project if you
  want CaddyTower to manage that process too.
- Keep the web project on the default catch-all host route unless you later move
  the split into explicit CaddyTower HTTP rules.

### Shared-host frontend plus API split

- Recreate the frontend as a **web** project and the backend/database as
  separate managed services as needed.
- Keep the frontend container on the default catch-all host route.
- Add the backend split on the same host with:

  ```text
  @domains | path_prefix | /api | strip
  @domains | host
  ```

- If the backend also needs mounted host data, add those bind mounts to the
  backend project itself.

## 7. Clean up the old manual path

- After verifying the migrated hosts work through CaddyTower, stop editing
  `Caddyfile.shared`.
- Remove legacy per-app Caddyfile fragments only after their matching projects
  are visible and healthy in the dashboard.
- Remove legacy bind-mounted config files only after the matching CaddyTower
  project shows the same mount list and routing behavior from its project page.
- Keep the old file around as `.bak` until you have at least one successful
  nightly backup.

## 7a. What still stays out of scope

- In-place adoption of already-running external containers without recreating
  them.
- Raw arbitrary Caddy JSON editing from the UI.
- Docker named volumes, tmpfs mounts, and other advanced mount propagation flags.

## 8. Roll back if needed

- If a migrated hostname breaks, stop the affected CaddyTower project from the
  dashboard or container layer.
- Restore the previous Caddyfile-based route from the saved backup copy.
- Redeploy the original app container if the recreated settings drifted from the
  live configuration.
