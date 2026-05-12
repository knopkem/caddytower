#!/usr/bin/env bash

caddytower_log() {
  printf '%s\n' "$*"
}

caddytower_warn() {
  printf 'Warning: %s\n' "$*" >&2
}

caddytower_die() {
  printf 'Error: %s\n' "$*" >&2
  exit 1
}

caddytower_require_command() {
  local name="${1:-}"
  if [[ -z "${name}" ]]; then
    caddytower_die "internal error: missing command name"
  fi
  if ! command -v "${name}" >/dev/null 2>&1; then
    caddytower_die "${name} is required but was not found in PATH."
  fi
}

caddytower_require_docker() {
  caddytower_require_command docker

  if ! docker info >/dev/null 2>&1; then
    caddytower_die "docker is installed but the daemon is not reachable."
  fi

  if ! docker compose version >/dev/null 2>&1; then
    caddytower_die "docker compose is required. Install Docker with the Compose plugin."
  fi
}

caddytower_has_tty() {
  [[ -r /dev/tty && -w /dev/tty ]]
}

caddytower_prompt() {
  local message="${1:-}"
  local default_value="${2:-}"
  local response=""

  if ! caddytower_has_tty; then
    printf '%s' "${default_value}"
    return 0
  fi

  if [[ -n "${default_value}" ]]; then
    printf '%s [%s]: ' "${message}" "${default_value}" > /dev/tty
  else
    printf '%s: ' "${message}" > /dev/tty
  fi
  IFS= read -r response < /dev/tty || true
  if [[ -z "${response}" ]]; then
    response="${default_value}"
  fi
  printf '%s' "${response}"
}

caddytower_confirm() {
  local message="${1:-Are you sure?}"
  local default_answer="${2:-n}"
  local suffix="[y/N]"
  local response=""

  if [[ "${default_answer}" == "y" ]]; then
    suffix="[Y/n]"
  fi

  if ! caddytower_has_tty; then
    [[ "${default_answer}" == "y" ]]
    return
  fi

  printf '%s %s ' "${message}" "${suffix}" > /dev/tty
  IFS= read -r response < /dev/tty || true
  response="$(printf '%s' "${response}" | tr '[:upper:]' '[:lower:]')"
  if [[ -z "${response}" ]]; then
    response="${default_answer}"
  fi

  [[ "${response}" == "y" || "${response}" == "yes" ]]
}

caddytower_copy_if_missing() {
  local source_path="${1:-}"
  local dest_path="${2:-}"

  [[ -n "${source_path}" && -n "${dest_path}" ]] || caddytower_die "internal error: copy_if_missing needs source and destination"

  mkdir -p "$(dirname "${dest_path}")"
  if [[ -f "${dest_path}" ]]; then
    caddytower_log "Keeping existing ${dest_path}"
    return 1
  fi

  cp "${source_path}" "${dest_path}"
  caddytower_log "Created ${dest_path}"
}

caddytower_fetch_to_file() {
  local url="${1:-}"
  local dest_path="${2:-}"

  [[ -n "${url}" && -n "${dest_path}" ]] || caddytower_die "internal error: fetch_to_file needs url and destination"

  caddytower_require_command curl
  mkdir -p "$(dirname "${dest_path}")"
  curl --fail --silent --show-error --location "${url}" --output "${dest_path}"
}

caddytower_get_env_value() {
  local file_path="${1:-}"
  local key="${2:-}"

  [[ -f "${file_path}" ]] || return 0

  awk -v key="${key}" '
    index($0, key "=") == 1 {
      value = substr($0, index($0, "=") + 1)
    }
    END {
      if (value != "") {
        print value
      }
    }
  ' "${file_path}"
}

caddytower_set_env_value() {
  local file_path="${1:-}"
  local key="${2:-}"
  local value="${3:-}"
  local tmp_file

  [[ -n "${file_path}" && -n "${key}" ]] || caddytower_die "internal error: set_env_value needs file and key"

  tmp_file="$(mktemp)"
  awk -v key="${key}" -v value="${value}" '
    BEGIN {
      done = 0
    }
    index($0, key "=") == 1 {
      print key "=" value
      done = 1
      next
    }
    {
      print
    }
    END {
      if (!done) {
        print key "=" value
      }
    }
  ' "${file_path}" > "${tmp_file}"
  mv "${tmp_file}" "${file_path}"
}

caddytower_generate_master_key_if_needed() {
  local env_file="${1:-}"
  local current_value=""
  local generated_key=""

  [[ -n "${env_file}" ]] || caddytower_die "internal error: missing env file path"

  current_value="$(caddytower_get_env_value "${env_file}" "CADDYTOWER_MASTER_KEY")"
  if [[ -n "${current_value}" && "${current_value}" != "REPLACE_WITH_GENERATED_KEY" ]]; then
    return 0
  fi

  generated_key="$(head -c 32 /dev/urandom | base64 | tr -d '\n')"
  caddytower_set_env_value "${env_file}" "CADDYTOWER_MASTER_KEY" "${generated_key}"
  caddytower_log "Generated CADDYTOWER_MASTER_KEY"
}

caddytower_ensure_edge_network() {
  if ! docker network inspect edge >/dev/null 2>&1; then
    docker network create edge >/dev/null
    caddytower_log "Created Docker network: edge"
  fi
}

caddytower_configure_github_pem_mount() {
  local compose_file="${1:-}"
  local host_path="${2:-}"
  local container_path="${3:-}"
  local mount_line=""
  local tmp_file=""

  [[ -n "${compose_file}" && -n "${host_path}" && -n "${container_path}" ]] || caddytower_die "internal error: github mount needs compose file, host path, and container path"

  mount_line="      - ${host_path}:${container_path}:ro"
  if grep -Fqx "${mount_line}" "${compose_file}"; then
    return 0
  fi

  tmp_file="$(mktemp)"
  awk -v mount_line="${mount_line}" '
    {
      print
      if (!inserted && ($0 == "      - caddytower-data:/data" || $0 == "      # installer-managed mounts go here")) {
        print mount_line
        inserted = 1
      }
    }
    END {
      if (!inserted) {
        exit 1
      }
    }
  ' "${compose_file}" > "${tmp_file}" || {
    rm -f "${tmp_file}"
    return 1
  }
  mv "${tmp_file}" "${compose_file}"
}

caddytower_compose_up() {
  local target_dir="${1:-}"
  local compose_profiles=()

  [[ -n "${target_dir}" ]] || caddytower_die "internal error: missing target directory"

  if ! docker container inspect shared-caddy >/dev/null 2>&1; then
    compose_profiles+=(--profile bundled-caddy)
    caddytower_log "No shared-caddy container found; bootstrap will start the bundled Caddy service."
  fi

  if ! docker container inspect watchtower >/dev/null 2>&1; then
    compose_profiles+=(--profile bundled-watchtower)
    caddytower_log "No watchtower container found; bootstrap will start the bundled Watchtower service."
  fi

  (
    cd "${target_dir}"
    docker compose "${compose_profiles[@]}" up -d
  )
}

caddytower_print_access_summary() {
  local public_base_url="${1:-http://127.0.0.1:8080}"

  if [[ "${public_base_url}" == "http://127.0.0.1:8080" || "${public_base_url}" == "http://localhost:8080" ]]; then
    cat <<EOF

CaddyTower started.

Open it through an SSH tunnel first:
  ssh -L 8080:127.0.0.1:8080 <your-vps>

Then visit:
  http://127.0.0.1:8080/setup

EOF
    return 0
  fi

  cat <<EOF

CaddyTower started.

Open:
  ${public_base_url%/}/setup

EOF
}
