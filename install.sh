#!/usr/bin/env bash

set -euo pipefail

INSTALL_OWNER="${CADDYTOWER_INSTALL_OWNER:-knopkem}"
INSTALL_REPO="${CADDYTOWER_INSTALL_REPO:-caddytower}"
INSTALL_REF="${CADDYTOWER_INSTALL_REF:-}"
TARGET_DIR="${CADDYTOWER_INSTALL_TARGET_DIR:-/opt/caddytower}"
IMAGE_REF="${CADDYTOWER_IMAGE:-}"
PUBLIC_BASE_URL="${CADDYTOWER_PUBLIC_BASE_URL:-}"
ROOT_DOMAIN="${CADDYTOWER_ROOT_DOMAIN:-}"
GITHUB_APP_ID="${CADDYTOWER_GITHUB_APP_ID:-}"
GITHUB_APP_SLUG="${CADDYTOWER_GITHUB_APP_SLUG:-}"
GITHUB_WEBHOOK_SECRET="${CADDYTOWER_GITHUB_WEBHOOK_SECRET:-}"
GITHUB_PEM_HOST_PATH="${CADDYTOWER_GITHUB_PEM_HOST_PATH:-}"
GITHUB_PEM_CONTAINER_PATH="${CADDYTOWER_GITHUB_PEM_CONTAINER_PATH:-/run/secrets/github-app.pem}"
GITHUB_MODE="auto"
NONINTERACTIVE=0
REFRESH_ASSETS=0
SUPPLIED_GITHUB_VALUES_PRESENT=0

log() {
  printf '%s\n' "$*"
}

warn() {
  printf 'Warning: %s\n' "$*" >&2
}

die() {
  printf 'Error: %s\n' "$*" >&2
  exit 1
}

require_command() {
  local name="${1:-}"
  command -v "${name}" >/dev/null 2>&1 || die "${name} is required but was not found in PATH."
}

usage() {
  cat <<'EOF'
Usage: install.sh [options]

Options:
  --ref <git-ref>                 Install from this tag/branch instead of the latest release
  --owner <github-owner>          GitHub owner for raw assets (default: knopkem)
  --repo <github-repo>            GitHub repo for raw assets (default: caddytower)
  --target-dir <path>             Target directory (default: /opt/caddytower)
  --image <image-ref>             Controller image ref
  --public-base-url <url>         Public base URL for the admin UI
  --root-domain <domain>          Root domain for managed apps
  --enable-github                 Configure GitHub App integration now (advanced)
  --disable-github                Clear GitHub App env values from this install
  --github-app-id <id>            GitHub App ID
  --github-app-slug <slug>        GitHub App slug
  --github-webhook-secret <text>  GitHub webhook secret
  --github-pem-host-path <path>   Host path to the GitHub App PEM file
  --github-pem-container-path <path>
                                   In-container PEM mount path (default: /run/secrets/github-app.pem)
  --refresh-assets                Overwrite template-managed files from the chosen ref
  --yes                           Non-interactive mode; requires all mandatory values
  --help                          Show this help
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --ref)
      INSTALL_REF="${2:-}"
      shift 2
      ;;
    --owner)
      INSTALL_OWNER="${2:-}"
      shift 2
      ;;
    --repo)
      INSTALL_REPO="${2:-}"
      shift 2
      ;;
    --target-dir)
      TARGET_DIR="${2:-}"
      shift 2
      ;;
    --image)
      IMAGE_REF="${2:-}"
      shift 2
      ;;
    --public-base-url)
      PUBLIC_BASE_URL="${2:-}"
      shift 2
      ;;
    --root-domain)
      ROOT_DOMAIN="${2:-}"
      shift 2
      ;;
    --enable-github)
      GITHUB_MODE="enable"
      shift
      ;;
    --disable-github)
      GITHUB_MODE="disable"
      shift
      ;;
    --github-app-id)
      GITHUB_APP_ID="${2:-}"
      SUPPLIED_GITHUB_VALUES_PRESENT=1
      shift 2
      ;;
    --github-app-slug)
      GITHUB_APP_SLUG="${2:-}"
      SUPPLIED_GITHUB_VALUES_PRESENT=1
      shift 2
      ;;
    --github-webhook-secret)
      GITHUB_WEBHOOK_SECRET="${2:-}"
      SUPPLIED_GITHUB_VALUES_PRESENT=1
      shift 2
      ;;
    --github-pem-host-path)
      GITHUB_PEM_HOST_PATH="${2:-}"
      SUPPLIED_GITHUB_VALUES_PRESENT=1
      shift 2
      ;;
    --github-pem-container-path)
      GITHUB_PEM_CONTAINER_PATH="${2:-}"
      SUPPLIED_GITHUB_VALUES_PRESENT=1
      shift 2
      ;;
    --refresh-assets)
      REFRESH_ASSETS=1
      shift
      ;;
    --yes)
      NONINTERACTIVE=1
      shift
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      die "unknown option: $1"
      ;;
  esac
done

require_command curl

if [[ -n "${GITHUB_APP_ID}${GITHUB_APP_SLUG}${GITHUB_WEBHOOK_SECRET}${GITHUB_PEM_HOST_PATH}" ]]; then
  SUPPLIED_GITHUB_VALUES_PRESENT=1
fi
if [[ "${GITHUB_MODE}" == "auto" && "${SUPPLIED_GITHUB_VALUES_PRESENT}" -eq 1 ]]; then
  GITHUB_MODE="enable"
fi

resolve_latest_release_ref() {
  local api_url="https://api.github.com/repos/${INSTALL_OWNER}/${INSTALL_REPO}/releases/latest"
  local response=""
  local tag=""

  if ! response="$(curl --fail --silent --show-error --location "${api_url}")"; then
    return 1
  fi

  tag="$(printf '%s' "${response}" | tr -d '\n' | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')"
  [[ -n "${tag}" ]] || return 1
  printf '%s' "${tag}"
}

if [[ -z "${INSTALL_REF}" ]]; then
  if INSTALL_REF="$(resolve_latest_release_ref)"; then
    log "Using release ${INSTALL_REF}"
  else
    if [[ "${NONINTERACTIVE}" -eq 1 ]]; then
      die "could not resolve the latest release tag. Re-run with --ref <tag> (or explicitly --ref main)."
    fi
    warn "Could not resolve the latest release tag for ${INSTALL_OWNER}/${INSTALL_REPO}."
    if [[ -r /dev/tty && -w /dev/tty ]]; then
      printf 'Install from main instead? [y/N] ' > /dev/tty
      IFS= read -r fallback_choice < /dev/tty || true
      fallback_choice="$(printf '%s' "${fallback_choice:-}" | tr '[:upper:]' '[:lower:]')"
      if [[ "${fallback_choice}" == "y" || "${fallback_choice}" == "yes" ]]; then
        INSTALL_REF="main"
      else
        die "installation cancelled"
      fi
    else
      die "could not resolve the latest release tag and no tty is available for confirmation. Re-run with --ref <tag>."
    fi
  fi
fi

RAW_BASE_URL="https://raw.githubusercontent.com/${INSTALL_OWNER}/${INSTALL_REPO}/${INSTALL_REF}"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

HELPER_PATH="${TMP_DIR}/bootstrap-common.sh"
curl --fail --silent --show-error --location "${RAW_BASE_URL}/scripts/lib/bootstrap-common.sh" --output "${HELPER_PATH}" ||
  die "failed to download bootstrap helper from ${RAW_BASE_URL}"
# shellcheck source=/dev/null
source "${HELPER_PATH}"

default_image_ref() {
  printf 'ghcr.io/%s/caddytower:latest' "${INSTALL_OWNER}"
}

normalize_image_ref() {
  local value="${1:-}"
  if [[ -z "${value}" || "${value}" == *REPLACE_WITH_OWNER* ]]; then
    default_image_ref
    return 0
  fi
  printf '%s' "${value}"
}

random_secret() {
  head -c 32 /dev/urandom | base64 | tr -d '\n'
}

require_non_empty() {
  local label="${1:-value}"
  local value="${2:-}"
  [[ -n "${value}" ]] || die "${label} must not be empty"
}

require_https_url() {
  local label="${1:-url}"
  local value="${2:-}"
  [[ "${value}" == https://* ]] || die "${label} must start with https://"
}

suggested_public_base_url() {
  local root_domain="${1:-}"
  if [[ -n "${root_domain}" ]]; then
    printf 'https://caddytower.%s' "${root_domain}"
    return 0
  fi
  printf 'https://caddytower.example.com'
}

download_asset() {
  local remote_path="${1:-}"
  local dest_path="${2:-}"

  if [[ -f "${dest_path}" && "${REFRESH_ASSETS}" -ne 1 ]]; then
    caddytower_log "Keeping existing ${dest_path}"
    return 0
  fi

  caddytower_fetch_to_file "${RAW_BASE_URL}/${remote_path}" "${dest_path}"
  caddytower_log "Downloaded ${dest_path}"
}

existing_env=""
ENV_DEST="${TARGET_DIR}/caddytower.env"
COMPOSE_DEST="${TARGET_DIR}/docker-compose.yml"
CADDYFILE_DEST="${TARGET_DIR}/Caddyfile"

if [[ -f "${ENV_DEST}" ]]; then
  existing_env="${ENV_DEST}"
fi

existing_github_values_present=0
if [[ -n "${existing_env}" ]]; then
  if [[ -z "${GITHUB_APP_ID}" ]]; then
    GITHUB_APP_ID="$(caddytower_get_env_value "${existing_env}" "CADDYTOWER_GITHUB_APP_ID")"
  fi
  if [[ -z "${GITHUB_APP_SLUG}" ]]; then
    GITHUB_APP_SLUG="$(caddytower_get_env_value "${existing_env}" "CADDYTOWER_GITHUB_APP_SLUG")"
  fi
  if [[ -z "${GITHUB_WEBHOOK_SECRET}" ]]; then
    GITHUB_WEBHOOK_SECRET="$(caddytower_get_env_value "${existing_env}" "CADDYTOWER_GITHUB_WEBHOOK_SECRET")"
  fi
  existing_pem_container_path="$(caddytower_get_env_value "${existing_env}" "CADDYTOWER_GITHUB_APP_PRIVATE_KEY_PATH")"
  if [[ -z "${GITHUB_PEM_CONTAINER_PATH}" || "${GITHUB_PEM_CONTAINER_PATH}" == "/run/secrets/github-app.pem" ]]; then
    if [[ -n "${existing_pem_container_path}" ]]; then
      GITHUB_PEM_CONTAINER_PATH="${existing_pem_container_path}"
    fi
  fi
  if [[ -n "${GITHUB_APP_ID}${GITHUB_APP_SLUG}${existing_pem_container_path}${GITHUB_WEBHOOK_SECRET}" ]]; then
    existing_github_values_present=1
  fi
fi

if [[ -z "${IMAGE_REF}" ]]; then
  if [[ -n "${existing_env}" ]]; then
    IMAGE_REF="$(caddytower_get_env_value "${existing_env}" "CADDYTOWER_IMAGE")"
  fi
fi
IMAGE_REF="$(normalize_image_ref "${IMAGE_REF}")"

if [[ -z "${PUBLIC_BASE_URL}" && -n "${existing_env}" ]]; then
  PUBLIC_BASE_URL="$(caddytower_get_env_value "${existing_env}" "CADDYTOWER_PUBLIC_BASE_URL")"
fi
PUBLIC_BASE_URL="${PUBLIC_BASE_URL:-http://127.0.0.1:8080}"

if [[ -z "${ROOT_DOMAIN}" && -n "${existing_env}" ]]; then
  ROOT_DOMAIN="$(caddytower_get_env_value "${existing_env}" "CADDYTOWER_ROOT_DOMAIN")"
fi

ENABLE_GITHUB="0"
if [[ "${GITHUB_MODE}" == "enable" || "${GITHUB_MODE}" == "auto" && "${existing_github_values_present}" -eq 1 ]]; then
  ENABLE_GITHUB="1"
fi

if [[ "${NONINTERACTIVE}" -eq 0 ]]; then
  caddytower_log
  caddytower_log "CaddyTower installer"
  caddytower_log "  source repo: ${INSTALL_OWNER}/${INSTALL_REPO}"
  caddytower_log "  source ref:  ${INSTALL_REF}"
  caddytower_log

  TARGET_DIR="$(caddytower_prompt "Target directory" "${TARGET_DIR}")"
  IMAGE_REF="$(normalize_image_ref "$(caddytower_prompt "CaddyTower controller image" "${IMAGE_REF}")")"

  while [[ -z "${ROOT_DOMAIN}" ]]; do
    ROOT_DOMAIN="$(caddytower_prompt "Root domain for managed apps" "${ROOT_DOMAIN:-example.com}")"
    if [[ -z "${ROOT_DOMAIN}" ]]; then
      caddytower_warn "Root domain is required for a guided install."
    fi
  done

  if [[ "${PUBLIC_BASE_URL}" == "http://127.0.0.1:8080" || "${PUBLIC_BASE_URL}" == "http://localhost:8080" ]]; then
    if caddytower_confirm "Use local SSH-tunnel bootstrap first?" "y"; then
      PUBLIC_BASE_URL="http://127.0.0.1:8080"
    else
      while true; do
        PUBLIC_BASE_URL="$(caddytower_prompt "Final public HTTPS admin URL" "$(suggested_public_base_url "${ROOT_DOMAIN}")")"
        if [[ "${PUBLIC_BASE_URL}" == https://* ]]; then
          break
        fi
        caddytower_warn "The public admin URL must start with https://"
      done
    fi
  else
    PUBLIC_BASE_URL="$(caddytower_prompt "Public base URL" "${PUBLIC_BASE_URL}")"
  fi

  if [[ "${GITHUB_MODE}" == "enable" ]]; then
    ENABLE_GITHUB="1"
    if [[ "${PUBLIC_BASE_URL}" == "http://127.0.0.1:8080" || "${PUBLIC_BASE_URL}" == "http://localhost:8080" ]]; then
      caddytower_warn "GitHub webhooks need the final public HTTPS admin URL."
      while true; do
        PUBLIC_BASE_URL="$(caddytower_prompt "Final public HTTPS admin URL" "$(suggested_public_base_url "${ROOT_DOMAIN}")")"
        if [[ "${PUBLIC_BASE_URL}" == https://* ]]; then
          break
        fi
        caddytower_warn "The public admin URL must start with https://"
      done
    fi
    while [[ -z "${GITHUB_APP_ID}" ]]; do
      GITHUB_APP_ID="$(caddytower_prompt "GitHub App ID" "${GITHUB_APP_ID}")"
    done
    GITHUB_APP_SLUG="$(caddytower_prompt "GitHub App slug" "${GITHUB_APP_SLUG:-caddytower}")"
    GITHUB_WEBHOOK_SECRET="$(caddytower_prompt "GitHub webhook secret" "${GITHUB_WEBHOOK_SECRET:-$(random_secret)}")"

    while true; do
      GITHUB_PEM_HOST_PATH="$(caddytower_prompt "Host path to the GitHub App PEM file" "${GITHUB_PEM_HOST_PATH:-${TARGET_DIR}/secrets/github-app.pem}")"
      if [[ -f "${GITHUB_PEM_HOST_PATH}" ]]; then
        break
      fi
      caddytower_warn "No file exists at ${GITHUB_PEM_HOST_PATH}."
      if caddytower_confirm "Skip GitHub App setup for now?" "y"; then
        ENABLE_GITHUB="0"
        GITHUB_APP_ID=""
        GITHUB_APP_SLUG=""
        GITHUB_WEBHOOK_SECRET=""
        GITHUB_PEM_HOST_PATH=""
        break
      fi
    done
  fi

  if [[ "${ENABLE_GITHUB}" == "1" ]]; then
    GITHUB_PEM_CONTAINER_PATH="$(caddytower_prompt "In-container path for the GitHub App PEM" "${GITHUB_PEM_CONTAINER_PATH}")"
  fi

  cat >/dev/tty <<EOF

Summary
  Install ref:      ${INSTALL_REF}
  Target dir:       ${TARGET_DIR}
  Controller image: ${IMAGE_REF}
  Public base URL:  ${PUBLIC_BASE_URL}
  Root domain:      ${ROOT_DOMAIN}
  GitHub import:    $( [[ "${ENABLE_GITHUB}" == "1" ]] && printf 'enabled/preserved' || printf 'configure later in Settings' )
EOF
  if [[ "${ENABLE_GITHUB}" == "1" ]]; then
    cat >/dev/tty <<EOF
  GitHub App ID:    ${GITHUB_APP_ID}
  GitHub App slug:  ${GITHUB_APP_SLUG}
  PEM host path:    ${GITHUB_PEM_HOST_PATH}
EOF
  fi
  printf '\n' > /dev/tty
  caddytower_confirm "Continue with installation?" "y" || die "installation cancelled"
else
  require_non_empty "target directory" "${TARGET_DIR}"
  require_non_empty "CADDYTOWER_IMAGE" "${IMAGE_REF}"
  require_non_empty "CADDYTOWER_ROOT_DOMAIN" "${ROOT_DOMAIN}"
  if [[ "${PUBLIC_BASE_URL}" != "http://127.0.0.1:8080" && "${PUBLIC_BASE_URL}" != "http://localhost:8080" ]]; then
    require_https_url "CADDYTOWER_PUBLIC_BASE_URL" "${PUBLIC_BASE_URL}"
  fi
  if [[ "${GITHUB_MODE}" == "enable" ]]; then
    require_non_empty "CADDYTOWER_GITHUB_APP_ID" "${GITHUB_APP_ID}"
    require_non_empty "CADDYTOWER_GITHUB_APP_SLUG" "${GITHUB_APP_SLUG}"
    require_non_empty "CADDYTOWER_GITHUB_WEBHOOK_SECRET" "${GITHUB_WEBHOOK_SECRET}"
    require_non_empty "CADDYTOWER_GITHUB_PEM_HOST_PATH" "${GITHUB_PEM_HOST_PATH}"
    require_https_url "CADDYTOWER_PUBLIC_BASE_URL" "${PUBLIC_BASE_URL}"
    [[ -f "${GITHUB_PEM_HOST_PATH}" ]] || die "GitHub PEM file not found at ${GITHUB_PEM_HOST_PATH}"
  fi
fi

ENV_DEST="${TARGET_DIR}/caddytower.env"
COMPOSE_DEST="${TARGET_DIR}/docker-compose.yml"
CADDYFILE_DEST="${TARGET_DIR}/Caddyfile"
IMAGE_REF="$(normalize_image_ref "${IMAGE_REF}")"

caddytower_require_docker
caddytower_prepare_target_dir "${TARGET_DIR}"
caddytower_ensure_edge_network

download_asset "deploy/docker-compose.caddytower.yml" "${COMPOSE_DEST}"
download_asset "deploy/caddytower.env.example" "${ENV_DEST}"
download_asset "deploy/Caddyfile" "${CADDYFILE_DEST}"
caddytower_ensure_watchtower_api_version "${COMPOSE_DEST}"

caddytower_set_env_value "${ENV_DEST}" "CADDYTOWER_IMAGE" "${IMAGE_REF}"
caddytower_set_env_value "${ENV_DEST}" "CADDYTOWER_PUBLIC_BASE_URL" "${PUBLIC_BASE_URL}"
caddytower_set_env_value "${ENV_DEST}" "CADDYTOWER_ROOT_DOMAIN" "${ROOT_DOMAIN}"
caddytower_generate_master_key_if_needed "${ENV_DEST}"

if [[ "${GITHUB_MODE}" == "enable" ]]; then
  caddytower_set_env_value "${ENV_DEST}" "CADDYTOWER_GITHUB_APP_ID" "${GITHUB_APP_ID}"
  caddytower_set_env_value "${ENV_DEST}" "CADDYTOWER_GITHUB_APP_SLUG" "${GITHUB_APP_SLUG}"
  caddytower_set_env_value "${ENV_DEST}" "CADDYTOWER_GITHUB_APP_PRIVATE_KEY_PATH" "${GITHUB_PEM_CONTAINER_PATH}"
  caddytower_set_env_value "${ENV_DEST}" "CADDYTOWER_GITHUB_WEBHOOK_SECRET" "${GITHUB_WEBHOOK_SECRET}"
  caddytower_configure_github_pem_mount "${COMPOSE_DEST}" "${GITHUB_PEM_HOST_PATH}" "${GITHUB_PEM_CONTAINER_PATH}" ||
    die "failed to add the GitHub App PEM mount to ${COMPOSE_DEST}"
elif [[ "${GITHUB_MODE}" == "disable" ]]; then
  caddytower_set_env_value "${ENV_DEST}" "CADDYTOWER_GITHUB_APP_ID" ""
  caddytower_set_env_value "${ENV_DEST}" "CADDYTOWER_GITHUB_APP_SLUG" ""
  caddytower_set_env_value "${ENV_DEST}" "CADDYTOWER_GITHUB_APP_PRIVATE_KEY_PATH" ""
  caddytower_set_env_value "${ENV_DEST}" "CADDYTOWER_GITHUB_WEBHOOK_SECRET" ""
fi

caddytower_compose_up "${TARGET_DIR}"
caddytower_print_access_summary "${PUBLIC_BASE_URL}"

if [[ "${GITHUB_MODE}" == "enable" ]]; then
  cat <<EOF
GitHub App follow-up
  Homepage URL: ${PUBLIC_BASE_URL}
  Webhook URL:  ${PUBLIC_BASE_URL%/}/api/webhooks/github
  Permissions:  Metadata (read), Contents (read+write), Pull requests (read+write)
  Events:       installation, installation_repositories

After the stack is up, open Settings, click "Connect on GitHub", install the app,
then refresh the page after GitHub delivers the installation webhook.

EOF
elif [[ "${ENABLE_GITHUB}" == "1" ]]; then
  cat <<EOF
Existing GitHub App configuration was preserved.

After first login, open Settings to review the GitHub connection status and
finish any remaining GitHub App installation steps there.

EOF
else
  cat <<EOF
GitHub import was left for later.

After first login, open Settings and follow the GitHub setup guide there when
you want repo-based imports. The default install path keeps the first-run setup
focused on getting CaddyTower online.

EOF
fi
