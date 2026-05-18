#!/usr/bin/env bash

declare -a CADDYTOWER_DOCKER_CMD=(docker)

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

caddytower_run_sudo() {
  if [[ "${EUID:-$(id -u)}" -eq 0 ]]; then
    "$@"
    return
  fi
  sudo "$@"
}

caddytower_request_sudo() {
  local reason="${1:-perform privileged operations}"
  if [[ "${EUID:-$(id -u)}" -eq 0 ]]; then
    return 0
  fi
  caddytower_require_command sudo
  caddytower_log "Requesting sudo to ${reason}..."
  if ! sudo -v; then
    caddytower_die "sudo access is required to ${reason}"
  fi
}

caddytower_docker() {
  "${CADDYTOWER_DOCKER_CMD[@]}" "$@"
}

caddytower_require_docker() {
  caddytower_require_command docker

  if docker info >/dev/null 2>&1; then
    CADDYTOWER_DOCKER_CMD=(docker)
  else
    if [[ "${EUID:-$(id -u)}" -ne 0 ]]; then
      caddytower_request_sudo "access Docker"
      if sudo docker info >/dev/null 2>&1; then
        CADDYTOWER_DOCKER_CMD=(sudo docker)
      else
        caddytower_die "docker is installed but the daemon is not reachable."
      fi
    else
      caddytower_die "docker is installed but the daemon is not reachable."
    fi
  fi

  if ! caddytower_docker compose version >/dev/null 2>&1; then
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

caddytower_prepare_target_dir() {
  local target_dir="${1:-}"
  local probe_file=""

  [[ -n "${target_dir}" ]] || caddytower_die "internal error: missing target directory"

  mkdir -p "${target_dir}" 2>/dev/null || true
  probe_file="${target_dir}/.caddytower-write-test.$$"
  if [[ -d "${target_dir}" ]] && : > "${probe_file}" 2>/dev/null; then
    rm -f "${probe_file}"
    return 0
  fi

  caddytower_request_sudo "create and manage ${target_dir}"
  caddytower_run_sudo mkdir -p "${target_dir}"
  if [[ "${EUID:-$(id -u)}" -ne 0 ]]; then
    caddytower_run_sudo chown -R "$(id -un):$(id -gn)" "${target_dir}"
  fi

  probe_file="${target_dir}/.caddytower-write-test.$$"
  if [[ -d "${target_dir}" ]] && : > "${probe_file}" 2>/dev/null; then
    rm -f "${probe_file}"
    return 0
  fi

  caddytower_die "could not make ${target_dir} writable for the current user"
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
  if ! caddytower_docker network inspect edge >/dev/null 2>&1; then
    caddytower_docker network create edge >/dev/null
    caddytower_log "Created Docker network: edge"
  fi
}

caddytower_detect_socket_gid() {
  local socket_path="${1:-/var/run/docker.sock}"
  local gid=""

  [[ -S "${socket_path}" ]] || caddytower_die "Docker socket not found at ${socket_path}"

  gid="$(stat -c '%g' "${socket_path}" 2>/dev/null || true)"
  if [[ -z "${gid}" ]]; then
    gid="$(stat -f '%g' "${socket_path}" 2>/dev/null || true)"
  fi
  if [[ -z "${gid}" ]]; then
    gid="$(ls -ln "${socket_path}" | awk '{print $4}')"
  fi
  [[ -n "${gid}" ]] || caddytower_die "failed to detect the Docker socket group id for ${socket_path}"

  printf '%s' "${gid}"
}

caddytower_service_container_exists() {
  local service_name="${1:-}"

  [[ -n "${service_name}" ]] || caddytower_die "internal error: missing service name"

  caddytower_docker ps -a --format '{{.Names}}' | awk -v name="${service_name}" '
    $0 ~ ("(^|.*[-_])" name "([-_][0-9]+)?$") {
      found = 1
    }
    END {
      exit found ? 0 : 1
    }
  '
}

caddytower_ensure_watchtower_api_version() {
  local compose_file="${1:-}"
  local tmp_file=""

  [[ -n "${compose_file}" ]] || caddytower_die "internal error: missing compose file path"
  [[ -f "${compose_file}" ]] || return 0

  if ! grep -q '^  watchtower:$' "${compose_file}"; then
    return 0
  fi
  if grep -q 'DOCKER_API_VERSION:' "${compose_file}"; then
    return 0
  fi

  tmp_file="$(mktemp)"
  awk '
    BEGIN {
      in_watchtower = 0
      inserted = 0
    }
    /^  watchtower:$/ {
      in_watchtower = 1
    }
    in_watchtower && /^    networks:$/ && !inserted {
      print "    environment:"
      print "      DOCKER_API_VERSION: \"1.44\""
      inserted = 1
    }
    /^  [^ ]/ && $0 != "  watchtower:" && in_watchtower {
      in_watchtower = 0
    }
    {
      print
    }
    END {
      if (!inserted) {
        exit 1
      }
    }
  ' "${compose_file}" > "${tmp_file}" || {
    rm -f "${tmp_file}"
    caddytower_die "failed to patch watchtower API compatibility in ${compose_file}"
  }
  mv "${tmp_file}" "${compose_file}"
  caddytower_log "Patched ${compose_file} with DOCKER_API_VERSION for Watchtower compatibility"
}

caddytower_ensure_controller_docker_group() {
  local compose_file="${1:-}"
  local socket_path="${2:-/var/run/docker.sock}"
  local gid=""
  local tmp_file=""

  [[ -n "${compose_file}" ]] || caddytower_die "internal error: missing compose file path"
  [[ -f "${compose_file}" ]] || return 0

  if ! grep -q '^  caddytower:$' "${compose_file}"; then
    return 0
  fi
  gid="$(caddytower_detect_socket_gid "${socket_path}")"

  tmp_file="$(mktemp)"
  awk -v gid="${gid}" '
    BEGIN {
      in_caddytower = 0
      inserted = 0
      skipping_group_values = 0
    }
    /^  caddytower:$/ {
      in_caddytower = 1
    }
    in_caddytower && /^    group_add:$/ {
      if (!inserted) {
        print "    group_add:"
        print "      - \"" gid "\""
        inserted = 1
      }
      skipping_group_values = 1
      next
    }
    in_caddytower && skipping_group_values {
      if ($0 ~ /^      - /) {
        next
      }
      skipping_group_values = 0
    }
    in_caddytower && /^    volumes:$/ && !inserted {
      print "    group_add:"
      print "      - \"" gid "\""
      inserted = 1
    }
    /^  [^ ]/ && $0 != "  caddytower:" && in_caddytower {
      in_caddytower = 0
      skipping_group_values = 0
    }
    {
      print
    }
    END {
      if (!inserted) {
        exit 1
      }
    }
  ' "${compose_file}" > "${tmp_file}" || {
    rm -f "${tmp_file}"
    caddytower_die "failed to patch Docker socket group access in ${compose_file}"
  }
  mv "${tmp_file}" "${compose_file}"
  caddytower_log "Patched ${compose_file} with Docker socket group access for CaddyTower"
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

  if ! caddytower_service_container_exists shared-caddy; then
    compose_profiles+=(--profile bundled-caddy)
    caddytower_log "No shared-caddy container found; bootstrap will start the bundled Caddy service."
  fi

  if ! caddytower_service_container_exists watchtower; then
    compose_profiles+=(--profile bundled-watchtower)
    caddytower_log "No watchtower container found; bootstrap will start the bundled Watchtower service."
  fi

  (
    cd "${target_dir}"
    caddytower_docker compose "${compose_profiles[@]}" up -d
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
