#!/usr/bin/env bash

set -euo pipefail

TARGET_DIR="${1:-/opt/caddytower}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMMON_LIB="${REPO_ROOT}/scripts/lib/bootstrap-common.sh"
COMPOSE_SRC="${REPO_ROOT}/deploy/docker-compose.caddytower.yml"
ENV_SRC="${REPO_ROOT}/deploy/caddytower.env.example"
CADDYFILE_SRC="${REPO_ROOT}/deploy/Caddyfile"
COMPOSE_DEST="${TARGET_DIR}/docker-compose.yml"
ENV_DEST="${TARGET_DIR}/caddytower.env"
CADDYFILE_DEST="${TARGET_DIR}/Caddyfile"

if [[ ! -f "${COMMON_LIB}" ]]; then
  echo "Error: bootstrap helper library is missing: ${COMMON_LIB}" >&2
  exit 1
fi

# shellcheck source=./lib/bootstrap-common.sh
source "${COMMON_LIB}"

caddytower_require_docker

caddytower_prepare_target_dir "${TARGET_DIR}"
caddytower_ensure_edge_network

if [[ ! -f "${ENV_DEST}" ]]; then
  cp "${ENV_SRC}" "${ENV_DEST}"
  caddytower_log "Created ${ENV_DEST} from example"
fi

caddytower_copy_if_missing "${COMPOSE_SRC}" "${COMPOSE_DEST}" || true
caddytower_copy_if_missing "${CADDYFILE_SRC}" "${CADDYFILE_DEST}" || true
caddytower_ensure_watchtower_api_version "${COMPOSE_DEST}"
caddytower_ensure_controller_docker_group "${COMPOSE_DEST}"

caddytower_generate_master_key_if_needed "${ENV_DEST}"
caddytower_compose_up "${TARGET_DIR}"
caddytower_print_access_summary "$(caddytower_get_env_value "${ENV_DEST}" "CADDYTOWER_PUBLIC_BASE_URL")"
