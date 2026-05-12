#!/usr/bin/env bash

set -euo pipefail

TARGET_DIR="${1:-/opt/caddytower}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_SRC="${REPO_ROOT}/deploy/docker-compose.caddytower.yml"
ENV_SRC="${REPO_ROOT}/deploy/caddytower.env.example"
CADDYFILE_SRC="${REPO_ROOT}/deploy/Caddyfile"
COMPOSE_DEST="${TARGET_DIR}/docker-compose.yml"
ENV_DEST="${TARGET_DIR}/caddytower.env"
CADDYFILE_DEST="${TARGET_DIR}/Caddyfile"

if ! command -v docker >/dev/null 2>&1; then
  echo "Error: docker is required but was not found in PATH." >&2
  exit 1
fi

if ! docker info >/dev/null 2>&1; then
  echo "Error: docker is installed but the daemon is not reachable." >&2
  exit 1
fi

if ! docker compose version >/dev/null 2>&1; then
  echo "Error: docker compose is required. Install Docker with the Compose plugin." >&2
  exit 1
fi

mkdir -p "${TARGET_DIR}"

if ! docker network inspect edge >/dev/null 2>&1; then
  docker network create edge >/dev/null
  echo "Created Docker network: edge"
fi

cp "${COMPOSE_SRC}" "${COMPOSE_DEST}"
cp "${CADDYFILE_SRC}" "${CADDYFILE_DEST}"

if [[ ! -f "${ENV_DEST}" ]]; then
  cp "${ENV_SRC}" "${ENV_DEST}"
  echo "Created ${ENV_DEST} from example"
fi

if grep -q '^CADDYTOWER_MASTER_KEY=REPLACE_WITH_GENERATED_KEY$' "${ENV_DEST}"; then
  GENERATED_KEY="$(head -c 32 /dev/urandom | base64 | tr -d '\n')"
  sed -i.bak "s|^CADDYTOWER_MASTER_KEY=REPLACE_WITH_GENERATED_KEY$|CADDYTOWER_MASTER_KEY=${GENERATED_KEY}|" "${ENV_DEST}"
  rm -f "${ENV_DEST}.bak"
  echo "Generated CADDYTOWER_MASTER_KEY"
fi

if grep -q 'REPLACE_WITH_OWNER' "${ENV_DEST}"; then
  cat <<EOF

Bootstrap files are ready in ${TARGET_DIR}.

Next steps:
  1. Edit ${ENV_DEST} and replace:
     - CADDYTOWER_IMAGE
     - CADDYTOWER_PUBLIC_BASE_URL
     - CADDYTOWER_ROOT_DOMAIN
  2. Re-run this bootstrap script after saving the env file:
     $(basename "$0") ${TARGET_DIR}
  3. Open the UI through an SSH tunnel first:
     ssh -L 8080:127.0.0.1:8080 <your-vps>
     then visit http://127.0.0.1:8080/setup

EOF
  exit 0
fi

compose_profiles=()

if ! docker container inspect shared-caddy >/dev/null 2>&1; then
  compose_profiles+=(--profile bundled-caddy)
  echo "No shared-caddy container found; bootstrap will start the bundled Caddy service."
fi

if ! docker container inspect watchtower >/dev/null 2>&1; then
  compose_profiles+=(--profile bundled-watchtower)
  echo "No watchtower container found; bootstrap will start the bundled Watchtower service."
fi

(
  cd "${TARGET_DIR}"
  docker compose "${compose_profiles[@]}" up -d
)

cat <<EOF

CaddyTower started.

If you have not exposed it through Caddy yet, tunnel it locally:
  ssh -L 8080:127.0.0.1:8080 <your-vps>

Then visit:
  http://127.0.0.1:8080/setup

EOF
