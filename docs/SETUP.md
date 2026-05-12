# Setup tutorial

This guide explains the first install from a fresh VPS, what the important
configuration values mean, and how to enable optional features such as GitHub
import, backups, and email alerts.

## 1. What CaddyTower expects

CaddyTower is designed for a single VPS that already has:

- Docker
- the Docker Compose plugin
- a domain you control; Cloudflare is only needed if you want automatic DNS
  records

CaddyTower itself runs as a container. It does **not** build your apps on the
VPS. Instead, it deploys container images you already publish to a registry
(normally GHCR), or it helps you wire a GitHub repo to GHCR through the import
flow.

## 2. The three values everyone gets stuck on

After the first bootstrap run, CaddyTower writes `/opt/caddytower/caddytower.env`
and stops so you can fill in the key values.

| Variable | What it is | Example |
| --- | --- | --- |
| `CADDYTOWER_IMAGE` | The **controller image** for CaddyTower itself. This is **not** one of your app images. Docker Compose uses this to start the `caddytower` container. | `ghcr.io/knopkem/caddytower:latest` |
| `CADDYTOWER_PUBLIC_BASE_URL` | The public URL browsers and webhooks use to reach the CaddyTower admin UI. Most installs can keep the bootstrap default and let the app use `https://caddytower.<root-domain>` as the public admin hostname. | `https://caddytower.example.com` |
| `CADDYTOWER_ROOT_DOMAIN` | The base domain CaddyTower should manage for generated app subdomains. If you deploy `blog`, it becomes `blog.example.com`. | `example.com` |

### How these values relate

- **Admin URL**: where you open CaddyTower itself, usually something like
  `https://tower.example.com`
- **Root domain**: the domain used by your managed apps, usually something like
  `example.com`
- **Origin hostname**: the hostname or public IP your DNS records should point
  at; this is configured later in the **Settings** page, for example
  `vps.example.com` or `203.0.113.10`

Those are different things. A common setup is:

- `CADDYTOWER_PUBLIC_BASE_URL=https://caddytower.example.com`
- `CADDYTOWER_ROOT_DOMAIN=example.com`
- Settings → **Origin hostname** = `vps.example.com`

## 3. If you do not already have a CaddyTower image

`CADDYTOWER_IMAGE` must point to a published Linux container image. If you are
running your own fork, build and push it first:

```bash
docker buildx build \
  --platform linux/amd64 \
  -t ghcr.io/<your-user-or-org>/caddytower:latest \
  --push .
```

Then set:

```bash
CADDYTOWER_IMAGE=ghcr.io/<your-user-or-org>/caddytower:latest
```

If you already publish the image elsewhere, use that image reference instead.

## 4. Bootstrap the VPS

```bash
curl -fsSL https://raw.githubusercontent.com/knopkem/caddytower/main/install.sh | bash
```

This installer:

- checks that Docker and `docker compose` are available
- resolves the latest tagged release by default
- creates the external `edge` Docker network if needed
- copies `docker-compose.yml`, `Caddyfile`, and `caddytower.env`
- generates `CADDYTOWER_MASTER_KEY` on first run
- fills in the main setup values interactively
- leaves GitHub App setup for later by default so first login is simpler
- starts bundled `shared-caddy` and `watchtower` only when they are missing,
  including installs where existing containers use compose-generated names

If `/opt/caddytower` or Docker access requires elevated privileges, the
installer asks for `sudo` when needed.

If you explicitly want the moving branch instead of the latest tagged release:

```bash
curl -fsSL https://raw.githubusercontent.com/knopkem/caddytower/main/install.sh | bash -s -- --ref main
```

If you prefer working from a local checkout, the old bootstrap path still works:

```bash
git clone https://github.com/knopkem/caddytower /opt/caddytower-src
cd /opt/caddytower-src
./scripts/bootstrap-caddytower.sh /opt/caddytower
```

## 5. Fill in `caddytower.env`

The guided installer writes these values for you. If you are using the local
bootstrap path instead, set at minimum:

```dotenv
CADDYTOWER_IMAGE=ghcr.io/<owner>/caddytower:latest
CADDYTOWER_PUBLIC_BASE_URL=http://127.0.0.1:8080
CADDYTOWER_ROOT_DOMAIN=example.com
```

Use the local `http://127.0.0.1:8080` value only for the very first setup over
an SSH tunnel. After that, CaddyTower will present `https://caddytower.<root-domain>`
as the default public admin URL in the UI and manage the shared Caddy route for
that hostname automatically. The remaining requirement is that the DNS record
exists, either manually or through optional Cloudflare automation.

`CADDYTOWER_MASTER_KEY` is generated automatically by the bootstrap script. Keep
it private and stable. It encrypts project env values, TOTP secrets, database
credentials, and API tokens at rest.

Rerunning the installer is safe by default: it keeps existing install files
unless you explicitly refresh them, and it updates the prompted environment
values in place.

GitHub App setup is intentionally deferred by default. After the first login,
open **Settings** and follow the GitHub setup guide there when you actually want
repo imports.

## 6. First login

The controller binds to localhost only, so tunnel in:

```bash
ssh -L 8080:127.0.0.1:8080 ubuntu@your-vps
```

Then open:

```text
http://127.0.0.1:8080/setup
```

Create the first admin account and scan the TOTP QR code. After login, use the
dashboard's **Guided start** flow.

## 7. Settings page: what to fill in there

The **Settings** page contains runtime settings that are better stored in the
app database than in environment variables.

### Root domain

The base domain CaddyTower manages for generated app hostnames, such as
`api.example.com` or `cameos.example.com`.

### Origin hostname

The hostname or public IP your DNS should point at. Examples:

- `vps.example.com`
- `203.0.113.10`

This is the machine where shared Caddy is actually listening on ports 80/443.

### Cloudflare zone ID and API token

These are **optional**. Set them only if you want CaddyTower to create and
update DNS records automatically through Cloudflare.

- **Zone ID**: open the target domain in Cloudflare and copy the **Zone ID**
  from the overview page sidebar
- **API token**: in Cloudflare open **Profile → API Tokens**, create a token
  from the **Edit zone DNS** template or a custom token, and scope it to this
  zone with `Zone:Read` and `DNS:Edit`

If you leave them empty, CaddyTower still works. Just create the admin hostname
and app subdomain records yourself with your DNS provider.

## 8. GitHub App setup for Import from GitHub

This is optional. Skip it if you only want manual image deploys.

The recommended path is:

1. finish the first login
2. make sure CaddyTower has its final public HTTPS admin URL
3. open **Settings**
4. follow the GitHub setup guide shown in the app

The installer still supports an advanced opt-in path for GitHub setup if you
explicitly pass `--enable-github`, but that is no longer the default experience.

GitHub import only works when all four GitHub App environment variables are set
together:

```dotenv
CADDYTOWER_GITHUB_APP_ID=
CADDYTOWER_GITHUB_APP_SLUG=
CADDYTOWER_GITHUB_APP_PRIVATE_KEY_PATH=
CADDYTOWER_GITHUB_WEBHOOK_SECRET=
```

### Step 1: make sure your public URL is final

GitHub must be able to reach:

```text
https://<your-caddytower-host>/api/webhooks/github
```

So before enabling the GitHub App flow, make sure the public admin hostname
(normally `https://caddytower.<root-domain>`) is already reachable over HTTPS.

### Step 2: create the GitHub App

Create a new GitHub App in GitHub settings.

Use:

- **Homepage URL**: `https://tower.example.com`
- **Webhook URL**: `https://tower.example.com/api/webhooks/github`
- **Webhook secret**: the same value you will place in
  `CADDYTOWER_GITHUB_WEBHOOK_SECRET`

Repository permissions:

- **Metadata**: read
- **Contents**: read and write
- **Pull requests**: read and write

Subscribe to webhook events:

- `installation`
- `installation_repositories`

After creating the app:

- copy the numeric **App ID**
- copy the **App slug**
- generate a **private key** and download the PEM file

### Step 3: mount the private key into the container

The stock compose file does not know where your GitHub App PEM lives, so add a
read-only mount for it.

For example, place the PEM on the VPS:

```bash
mkdir -p /opt/caddytower/secrets
cp ~/Downloads/caddytower-github-app.pem /opt/caddytower/secrets/github-app.pem
```

Then edit `/opt/caddytower/docker-compose.yml` and add a volume to the
`caddytower` service:

```yaml
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - caddytower-data:/data
      - ./secrets/github-app.pem:/run/secrets/github-app.pem:ro
```

Set the path in `caddytower.env`:

```dotenv
CADDYTOWER_GITHUB_APP_PRIVATE_KEY_PATH=/run/secrets/github-app.pem
```

If you rerun the bootstrap script later, re-check any manual changes you made to
`/opt/caddytower/docker-compose.yml`, including this PEM mount.

### Step 4: fill in the GitHub environment values

Example:

```dotenv
CADDYTOWER_GITHUB_APP_ID=1234567
CADDYTOWER_GITHUB_APP_SLUG=caddytower
CADDYTOWER_GITHUB_APP_PRIVATE_KEY_PATH=/run/secrets/github-app.pem
CADDYTOWER_GITHUB_WEBHOOK_SECRET=replace-with-a-random-secret
```

If you use GitHub Enterprise Server or another GitHub-compatible deployment, set
these too:

```dotenv
CADDYTOWER_GITHUB_API_BASE_URL=https://github.example.com/api/v3
CADDYTOWER_GITHUB_WEB_BASE_URL=https://github.example.com
```

### Step 5: restart CaddyTower

```bash
cd /opt/caddytower
docker compose up -d
```

Then open **Settings**. If the GitHub App configuration is valid, you can click
**Connect on GitHub**, install the app for your account or org, and return to
the settings page. After GitHub sends the installation webhook, the installation
should appear there and `/projects/import` becomes available.

### What the import flow does

For supported repos, CaddyTower can:

- list connected repos from the GitHub App installation
- detect a root `Dockerfile`
- detect the first `EXPOSE` port
- detect an existing GHCR image workflow
- create the CaddyTower project
- create a PR that adds `.github/workflows/caddytower-deploy.yml` if needed

Compose-based repos are intentionally excluded from this flow and should use the
manual image path instead.

## 9. Optional features in `caddytower.env`

### Backups

```dotenv
CADDYTOWER_BACKUPS_ENABLED=true
CADDYTOWER_BACKUPS_RETENTION_DAYS=14
CADDYTOWER_BACKUPS_SCHEDULE_UTC=02:30
CADDYTOWER_BACKUPS_INCLUDE_ENGINE_DUMPS=true
```

- Disabled by default
- Stores archives under `/data/backups`
- Can include SQLite plus shared Postgres and MariaDB dumps

### VPS warnings

```dotenv
CADDYTOWER_VPS_WARNINGS_ENABLED=true
CADDYTOWER_VPS_RAM_FREE_WARN_PERCENT=15
CADDYTOWER_VPS_DISK_FREE_WARN_PERCENT=15
CADDYTOWER_VPS_WARNING_CHECK_MINUTES=15
CADDYTOWER_VPS_WARNING_COOLDOWN_MINUTES=360
```

These drive the RAM and disk warnings visible in the UI.

### Email alerts for VPS warnings

```dotenv
CADDYTOWER_SMTP_HOST=smtp.example.com
CADDYTOWER_SMTP_PORT=587
CADDYTOWER_SMTP_USERNAME=mailer
CADDYTOWER_SMTP_PASSWORD=replace-me
CADDYTOWER_SMTP_FROM=alerts@example.com
CADDYTOWER_SMTP_TO=you@example.com
```

Email delivery is active only when at least `CADDYTOWER_SMTP_HOST`,
`CADDYTOWER_SMTP_FROM`, and `CADDYTOWER_SMTP_TO` are set.

## 10. Values most people should leave alone

These defaults are usually correct:

- `CADDYTOWER_HTTP_ADDR=:8080`
- `CADDYTOWER_DATA_DIR=/data` inside the container
- `CADDYTOWER_CADDY_ADMIN_URL=http://shared-caddy:2019`

`DOCKER_HOST` is only needed when CaddyTower should talk to a non-default Docker
daemon.

## 11. Common setup mistakes

### "Why does CADDYTOWER_IMAGE need my image? I thought this repo is local."

Because Docker Compose runs CaddyTower as a container. The repo checkout gives
you the compose file and bootstrap script, but the running controller still
needs a container image reference.

### "Can I leave CADDYTOWER_PUBLIC_BASE_URL as 127.0.0.1?"

Only for the first SSH-tunnel setup. GitHub webhooks and real browser access
need the final public HTTPS URL. In the normal flow, the app derives that from
the root domain and the admin hostname DNS record.

### "Should CADDYTOWER_ROOT_DOMAIN be tower.example.com?"

Usually no. The root domain is normally the base domain for generated app
subdomains, for example `example.com`.

### "Why is GitHub import still unavailable?"

Usually one of these is missing:

- one of the four required GitHub App env vars
- the PEM file mount inside the container
- the public HTTPS URL and webhook route
- the GitHub App installation webhook has not been delivered yet

## 12. Related docs

- [`../README.md`](../README.md) — short overview and quickstart
- [`MIGRATION.md`](MIGRATION.md) — move an existing live VPS over without downtime
- [`../deploy/caddytower.env.example`](../deploy/caddytower.env.example) — example env file
