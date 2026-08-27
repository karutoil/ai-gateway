#!/usr/bin/env bash
# =============================================================================
# AI Gateway — install / update / uninstall manager
#
# Interactive TUI (no arguments) or subcommands for scripting:
#
#   install.sh                interactive menu
#   install.sh install        fresh install: latest release binary + .env wizard
#   install.sh update         pull the latest released binary, keep data & .env
#   install.sh uninstall      remove the gateway (asks whether to wipe data)
#   install.sh status         installed version vs latest release, service state
#
# Non-interactive / CI usage (env overrides):
#   GATEWAY_VERSION=v1.8.0    pin a release instead of latest
#   GATEWAY_INSTALL_DIR=/opt/ai-gateway
#   GATEWAY_REPO=karutoil/ai-gateway
#   GATEWAY_YES=1             accept every default without prompting
#   GATEWAY_PURGE_DATA=1      `uninstall` defaults to wiping data (still shown)
#
# The installed binary is fully self-contained (web UI embedded, SQLite
# statically linked): no Go, Node or system packages needed at install time.
#
# One-liner:
#   curl -fsSL https://raw.githubusercontent.com/karutoil/ai-gateway/main/install.sh | bash
# =============================================================================
set -euo pipefail

REPO="${GATEWAY_REPO:-karutoil/ai-gateway}"
PINNED_VERSION="${GATEWAY_VERSION:-}"
NONINTERACTIVE="${GATEWAY_YES:-0}"

# Global cleanup handle for temp dirs. EXIT traps fire after the registering
# function returned, so a `local` variable would already be out of scope —
# always point this at the live temp dir and let cleanup() do the rm.
CLEANUP_DIR=""
cleanup() { [ -n "$CLEANUP_DIR" ] && rm -rf "$CLEANUP_DIR"; return 0; }
trap cleanup EXIT

# ---------------------------------------------------------------------------
# Pretty output
# ---------------------------------------------------------------------------
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  C_B="\033[1m"; C_G="\033[32m"; C_Y="\033[33m"; C_R="\033[31m"; C_C="\033[36m"; C_0="\033[0m"
else
  C_B=""; C_G=""; C_Y=""; C_R=""; C_C=""; C_0=""
fi
info()  { printf '%b\n' "${C_G}==>${C_0} $*"; }
warn()  { printf '%b\n' "${C_Y}warn:${C_0} $*"; }
err()   { printf '%b\n' "${C_R}error:${C_0} $*" >&2; }
die()   { err "$*"; exit 1; }
hr()    { printf '%b\n' "${C_C}──────────────────────────────────────────────────${C_0}"; }

banner() {
  hr
  printf '%b\n' "${C_B}  AI Gateway — install manager${C_0}"
  hr
}

# ---------------------------------------------------------------------------
# Input helpers. Prompts read from the terminal even when the script is piped
# (curl | bash). With GATEWAY_YES=1 every prompt returns its default.
# ---------------------------------------------------------------------------
ask() { # ask "Prompt" "default" -> echoes answer (default on bare Enter)
  local prompt="$1" def="${2:-}" ans=""
  if [ "$NONINTERACTIVE" = "1" ]; then
    printf '%s' "$def"
    return
  fi
  local tty_in="/dev/stdin"
  if [ ! -t 0 ]; then
    tty_in="/dev/tty"
  fi
  if [ "$def" != "" ]; then
    printf '%b [%s] ' "$prompt" "$def" >&2
  else
    printf '%b ' "$prompt" >&2
  fi
  if ! read -r ans < "$tty_in" 2>/dev/null; then
    ans=""
  fi
  [ -z "$ans" ] && ans="$def"
  printf '%s' "$ans"
}

ask_yes_no() { # ask_yes_no "Prompt" "y|n" -> returns 0 for yes, 1 for no
  local answer
  if [ "${2:-n}" = "y" ]; then
    answer="$(ask "$1 [Y/n]" "y")"
  else
    answer="$(ask "$1 [y/N]" "n")"
  fi
  case "$(printf '%s' "$answer" | tr '[:upper:]' '[:lower:]')" in
    y|yes) return 0 ;;
    *) return 1 ;;
  esac
}

gen_secret() { # gen_secret <bytes> -> random hex string
  local n="$1"
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex "$n"
  else
    od -An -N"$n" -tx1 /dev/urandom | tr -d ' \n'
  fi
}

# ---------------------------------------------------------------------------
# Network helpers (curl or wget, no hard dependency on either)
# ---------------------------------------------------------------------------
http_fetch() { # http_fetch <url> <outfile|-> ; extra headers via HTTP_HEADERS
  local url="$1" out="$2"
  if command -v curl >/dev/null 2>&1; then
    if [ "$out" = "-" ]; then
      curl -fsSL $HTTP_HEADERS "$url"
    else
      curl -fsSL $HTTP_HEADERS -o "$out" "$url"
    fi
  elif command -v wget >/dev/null 2>&1; then
    local hdr_args=""
    for h in $HTTP_HEADERS; do hdr_args="$hdr_args --header=$h"; done
    if [ "$out" = "-" ]; then
      wget -qO- $hdr_args "$url"
    else
      wget -qO "$out" $hdr_args "$url"
    fi
  else
    die "need curl or wget to download releases"
  fi
}
HTTP_HEADERS=""

sha256_of() { # sha256_of <file> -> hash
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

latest_version() {
  # NOTE: word-split deliberately (no quotes); values must contain no spaces.
  HTTP_HEADERS="-H Accept:application/vnd.github+json"
  local json
  json="$(http_fetch "https://api.github.com/repos/${REPO}/releases/latest" -)" || {
    die "could not query the latest release for ${REPO} (404 = no releases published yet; other errors may be rate limits). Set GATEWAY_VERSION=vX.Y.Z to pin one."
  }
  printf '%s' "$json" | grep -o '"tag_name"[^,}]*' | head -n 1 | sed 's/.*"v\{0,1\}\([0-9][0-9.]*\).*/v\1/'
}

resolve_version() {
  if [ -n "$PINNED_VERSION" ]; then
    printf '%s' "$PINNED_VERSION"
  else
    latest_version
  fi
}

# ---------------------------------------------------------------------------
# Platform detection -> release asset coordinates
# ---------------------------------------------------------------------------
detect_platform() {
  OS="$(uname -s | tr '[:upper:]' '[:lower:]')" # linux / darwin
  case "$(uname -m)" in
    x86_64|amd64)  ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *) die "unsupported architecture: $(uname -m)" ;;
  esac
  [ "$OS" = "linux" ] || [ "$OS" = "darwin" ] || die "unsupported OS: $OS"
}

asset_name() { # asset_name <version>
  printf 'ai-gateway_%s_%s_%s.tar.gz' "$1" "$OS" "$ARCH"
}

# ---------------------------------------------------------------------------
# Download + verify a release tarball into $1 (temp dir)
# ---------------------------------------------------------------------------
download_release() {
  local tmp="$1" version="$2" asset url want got
  asset="$(asset_name "$version")"
  url="https://github.com/${REPO}/releases/download/${version}/${asset}"
  info "Downloading ${asset}"
  HTTP_HEADERS=""
  http_fetch "$url" "${tmp}/${asset}" || die "download failed: ${url}"
  HTTP_HEADERS=""
  # `set -e -o pipefail`: a grep miss here must not kill the installer, so the
  # assignment is guarded and validated below.
  if http_fetch "https://github.com/${REPO}/releases/download/${version}/checksums.txt" "${tmp}/checksums.txt" 2>/dev/null; then
    want="$(grep " ${asset}\$" "${tmp}/checksums.txt" 2>/dev/null | awk '{print $1}')" || true
    if [ -z "$want" ] && http_fetch "https://github.com/${REPO}/releases/download/${version}/${asset}.sha256" "${tmp}/${asset}.sha256" 2>/dev/null; then
      want="$(awk '{print $1}' "${tmp}/${asset}.sha256" 2>/dev/null)" || true
    fi
    got="$(sha256_of "${tmp}/${asset}")"
    if [ -z "$want" ]; then
      die "checksums.txt does not list ${asset} — refusing to install an unverified binary"
    fi
    if [ "$want" != "$got" ]; then
      die "checksum mismatch for ${asset} (want ${want}, got ${got})"
    fi
    info "Checksum verified (${got:0:16}...)"
  else
    warn "checksums.txt not available for this release — skipping verification"
  fi
  # The tarball may nest files under a versioned directory (CI layout:
  # ai-gateway_<ver>_<os>_<arch>/gateway) or be flat; find the binary either way.
  mkdir -p "${tmp}/extracted"
  tar -xzf "${tmp}/${asset}" -C "${tmp}/extracted"
  RELEASE_BINARY="$(find "${tmp}/extracted" -type f -name gateway | head -n 1)"
  [ -n "$RELEASE_BINARY" ] || die "tarball does not contain a gateway binary"
  RELEASE_DIR="$(dirname "$RELEASE_BINARY")"
}

# ---------------------------------------------------------------------------
# Locate an existing installation
# ---------------------------------------------------------------------------
find_install_dir() {
  # Explicit override only counts if something is actually installed there
  # (otherwise a fresh GATEWAY_INSTALL_DIR would look like an "existing install").
  if [ -n "${GATEWAY_INSTALL_DIR:-}" ] && [ -x "${GATEWAY_INSTALL_DIR}/gateway" ]; then
    printf '%s' "$GATEWAY_INSTALL_DIR"
    return
  fi
  local candidate
  for candidate in /opt/ai-gateway "${HOME}/.ai-gateway"; do
    if [ -x "${candidate}/gateway" ]; then
      printf '%s' "$candidate"
      return
    fi
  done
  printf ''
}

installed_version() { # installed_version <dir>
  local dir="$1"
  if [ -x "${dir}/gateway" ]; then
    "${dir}/gateway" version 2>/dev/null | awk '{print $2}' && return
  fi
  if [ -f "${dir}/VERSION" ]; then
    cat "${dir}/VERSION"
  fi
}

# ---------------------------------------------------------------------------
# systemd service management (root installs only)
# ---------------------------------------------------------------------------
SERVICE_NAME="ai-gateway"
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
SERVICE_USER="aigateway"

have_systemd() {
  [ "$(id -u)" -eq 0 ] && command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]
}

ensure_service_user() {
  if id "$SERVICE_USER" >/dev/null 2>&1; then return; fi
  if command -v useradd >/dev/null 2>&1; then
    useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER" 2>/dev/null \
      || useradd --system --no-create-home "$SERVICE_USER" 2>/dev/null || true
  elif command -v adduser >/dev/null 2>&1; then
    adduser --system --no-create-home --disabled-password "$SERVICE_USER" 2>/dev/null || true
  fi
}

write_service_unit() { # write_service_unit <dir>
  local dir="$1" protect_home="true"
  case "$dir" in /home/*|/root/*) protect_home="false" ;; esac
  cat > "$SERVICE_FILE" <<EOF
[Unit]
Description=AI Gateway (LLM API gateway)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${SERVICE_USER}
Group=${SERVICE_USER}
WorkingDirectory=${dir}
ExecStart=${dir}/gateway
Restart=on-failure
RestartSec=3
# The gateway loads .env from its own directory and persists state under
# WorkingDirectory; everything else can stay read-only.
NoNewPrivileges=true
ProtectSystem=full
ProtectHome=${protect_home}
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable "$SERVICE_NAME" >/dev/null 2>&1 || true
}

service_stop() {
  if have_systemd && systemctl list-unit-files | grep -q "^${SERVICE_NAME}"; then
    systemctl stop "$SERVICE_NAME" 2>/dev/null || true
  fi
}

service_start() { # returns non-zero if the restart failed (callers rely on this)
  if have_systemd && [ -f "$SERVICE_FILE" ]; then
    if ! systemctl restart "$SERVICE_NAME"; then
      return 1
    fi
    systemctl --no-pager status "$SERVICE_NAME" | head -n 5 || true
    return 0
  fi
  return 1
}

service_active() {
  have_systemd && [ -f "$SERVICE_FILE" ] && systemctl is-active --quiet "$SERVICE_NAME"
}

# gateway processes whose executable lives in <dir> — matched via /proc/<pid>/exe
# (command-line matching misses runs started as `cd <dir> && ./gateway`).
find_gateway_pids() { # find_gateway_pids <dir> -> space-separated pids
  local pid exe pids=""
  if [ -d /proc ] && command -v pgrep >/dev/null 2>&1; then
    for pid in $(pgrep -x gateway 2>/dev/null); do
      exe="$(readlink "/proc/$pid/exe" 2>/dev/null)" || continue
      case "$exe" in
        "$1/gateway"|"$1/gateway.bak"|"$1/gateway.new") pids="$pids $pid" ;;
      esac
    done
  fi
  printf '%s' "$pids"
}

# stop_gateway_processes <dir> — TERM, wait, then KILL if still alive
stop_gateway_processes() {
  local pids pid
  pids="$(find_gateway_pids "$1")"
  [ -n "$pids" ] || return 0
  # shellcheck disable=SC2086
  kill -TERM $pids 2>/dev/null || true
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    [ -z "$(find_gateway_pids "$1")" ] && return 0
    sleep 1
  done
  pids="$(find_gateway_pids "$1")"
  # shellcheck disable=SC2086
  [ -n "$pids" ] && kill -KILL $pids 2>/dev/null || true
  return 0
}

# ---------------------------------------------------------------------------
# .env wizard — every field has a default; bare Enter accepts it.
# ---------------------------------------------------------------------------
write_env_file() { # write_env_file <dir>
  local dir="$1" envpath="${1}/.env" port public_url admin_pw master_key jwt_secret
  local env_mode database_url redis_url cors log_bodies use_defaults

  echo ""
  info "${C_B}Configure .env${C_0} (bare ${C_B}Enter${C_0} accepts the default)"
  use_defaults_ask="$(ask_yes_no "Use recommended secure defaults for everything?" "y" && echo yes || echo no)"
  if [ "$use_defaults_ask" = "yes" ]; then
    use_defaults=1
  else
    use_defaults=0
  fi

  if [ "$use_defaults" = "1" ]; then
    env_mode="production"
    port="8080"
    admin_pw="$(gen_secret 16)"
    master_key="$(gen_secret 32)"
    jwt_secret="$(gen_secret 32)"
    public_url="http://localhost:${port}"
    database_url="./data/gateway.db"
    redis_url=""
    cors=""
    log_bodies="false"
  else
    env_mode="$(ask "ENV — production|dev" "production")"
    port="$(ask "PORT" "8080")"
    case "$port" in ''|*[!0-9]*) die "PORT must be numeric" ;; esac
    admin_pw="$(ask "ADMIN_PASSWORD (empty = generate strong random)" "")"
    [ -z "$admin_pw" ] && admin_pw="$(gen_secret 16)"
    master_key="$(ask "MASTER_KEY 64-hex (empty = generate)" "")"
    [ -z "$master_key" ] && master_key="$(gen_secret 32)"
    jwt_secret="$(ask "JWT_SECRET >=32 chars (empty = generate)" "")"
    [ -z "$jwt_secret" ] && jwt_secret="$(gen_secret 32)"
    public_url="$(ask "PUBLIC_URL (https://gw.example.com)" "http://localhost:${port}")"
    database_url="$(ask "DATABASE_URL" "./data/gateway.db")"
    redis_url="$(ask "REDIS_URL (empty = in-memory cache)" "")"
    cors="$(ask "CORS_ALLOWED_ORIGINS (empty = PUBLIC_URL origin)" "")"
    log_bodies="$(ask_yes_no "LOG_BODIES — log request bodies? (privacy)" "n" && echo true || echo false)"
  fi

  mkdir -p "$dir"
  umask 077
  cat > "$envpath" <<EOF
# AI Gateway configuration
# Generated by install.sh on $(date -u '+%Y-%m-%dT%H:%MZ')

# production enables strength checks for secrets below; dev is permissive.
ENV=${env_mode}

# HTTP listener port
PORT=${port}

# Dashboard admin password (${admin_pw})
ADMIN_PASSWORD=${admin_pw}

# 32-byte hex keys used to encrypt stored provider credentials / sign sessions
MASTER_KEY=${master_key}
JWT_SECRET=${jwt_secret}

# Public URL used for CORS + links (reverse proxy / tunnel). Empty = "*".
PUBLIC_URL=${public_url}
# Comma-separated extra allowed origins, e.g. https://app.example.com
CORS_ALLOWED_ORIGINS=${cors}
# Comma-separated trusted proxy IPs/CIDRs for X-Forwarded-* handling
#TRUSTED_PROXIES=

# SQLite database (relative paths resolve against the install directory)
DATABASE_URL=${database_url}
# Optional Redis for shared cache + rate limiting: redis://host:6379
REDIS_URL=${redis_url}

# Debug/observability
LOG_BODIES=${log_bodies}
#LOG_RETENTION_DAYS=30
EOF

  echo ""
  info "Wrote ${envpath} (permissions 0600)"
  printf '%b\n' "  ${C_Y}ADMIN_PASSWORD: ${C_B}${admin_pw}${C_0}"
  printf '%b\n' "  ${C_Y}Save it now — it is shown only this once.${C_0}"
}

# ---------------------------------------------------------------------------
# install
# ---------------------------------------------------------------------------
cmd_install() {
  detect_platform
  local dir existing
  existing="$(find_install_dir)"
  if [ -n "$existing" ]; then
    warn "existing installation found at ${existing}"
    if ask_yes_no "Update that installation instead of creating a new one?" "y"; then
      cmd_update
      return
    fi
  fi

  banner
  printf '%b\n' "  Platform : ${OS}/${ARCH}"
  printf '%b\n' "  Repo     : ${REPO}"
  hr

  local default_dir="${GATEWAY_INSTALL_DIR:-}"
  if [ -z "$default_dir" ]; then
    default_dir="/opt/ai-gateway"
    [ "$(id -u)" -ne 0 ] && default_dir="${HOME}/.ai-gateway"
  fi
  dir="$(ask "Install directory" "$default_dir")"
  [ -n "$dir" ] || die "install directory required"

  local version
  version="$(resolve_version)"
  [ -n "$version" ] || die "could not resolve a release version"
  info "Installing version ${C_B}${version}${C_0}"

  local tmp
  CLEANUP_DIR="$(mktemp -d)"
  tmp="$CLEANUP_DIR"
  download_release "$tmp" "$version"

  mkdir -p "$dir"
  # Keep an existing .env and data across reinstalls.
  if [ -f "${dir}/.env" ]; then
    cp "${dir}/.env" "${tmp}/.env.keep"
  fi
  install -m 0755 "$RELEASE_BINARY" "${dir}/gateway"
  cp "${RELEASE_DIR}/.env.example" "${dir}/.env.example" 2>/dev/null || true
  printf '%s\n' "$version" > "${dir}/VERSION"
  if [ -f "${tmp}/.env.keep" ]; then
    cp "${tmp}/.env.keep" "${dir}/.env"
    info "Kept existing .env"
  fi

  if [ ! -f "${dir}/.env" ]; then
    if ask_yes_no "Set up .env now? (No = copy defaults, edit later)" "y"; then
      write_env_file "$dir"
    else
      cp "${dir}/.env.example" "${dir}/.env" 2>/dev/null || \
        { echo "ENV=dev" > "${dir}/.env"; echo "PORT=8080" >> "${dir}/.env"; }
      warn "Using default .env — the dashboard admin password is 'admin123'. Change it."
    fi
  fi

  # Root + systemd: run as a dedicated system user and enable the service.
  local used_systemd=0
  if have_systemd; then
    if ask_yes_no "Install and start a systemd service (${SERVICE_NAME})?" "y"; then
      ensure_service_user
      chown -R "${SERVICE_USER}:${SERVICE_USER}" "$dir" 2>/dev/null || chown -R "$SERVICE_USER" "$dir" || true
      chmod 750 "$dir"
      write_service_unit "$dir"
      used_systemd=1
    fi
  fi

  echo ""
  hr
  if [ "$used_systemd" = "1" ]; then
    service_start || die "service failed to start — check: journalctl -u ${SERVICE_NAME} -n 50"
    info "${C_G}Installed and running.${C_0}"
    printf '%b\n' "  Service : systemctl ${C_C}status|restart|stop${C_0} ${SERVICE_NAME}"
    printf '%b\n' "  Logs    : journalctl -u ${SERVICE_NAME} -f"
  else
    info "${C_G}Installed to ${dir}.${C_0}"
    printf '%b\n' "  Start it with:"
    printf '%b\n' "    ${C_C}cd ${dir} && ./gateway${C_0}"
  fi
  hr
}

# ---------------------------------------------------------------------------
# update
# ---------------------------------------------------------------------------
cmd_update() {
  detect_platform
  local dir
  dir="$(find_install_dir)"
  [ -n "$dir" ] || die "no installation found — run '$0 install' first"

  local current version
  current="$(installed_version "$dir" || true)"
  version="$(resolve_version)"
  [ -n "$version" ] || die "could not resolve a release version"

  info "Installed: ${current:-unknown}   Latest: ${C_B}${version}${C_0}"
  if [ "$current" = "$version" ] && [ "${FORCE_UPDATE:-0}" != "1" ]; then
    info "Already up to date. (FORCE_UPDATE=1 to reinstall)"
    return
  fi

  local tmp
  CLEANUP_DIR="$(mktemp -d)"
  tmp="$CLEANUP_DIR"
  download_release "$tmp" "$version"

  local was_active=0 was_running=0
  if service_active; then was_active=1; fi
  if [ -n "$(find_gateway_pids "$dir")" ]; then
    was_running=1
  fi

  info "Stopping gateway..."
  if [ "$was_active" = "1" ]; then service_stop; fi
  if [ "$was_running" = "1" ]; then
    stop_gateway_processes "$dir"
  fi

  # Backup -> swap -> verify; roll back if the new binary cannot even start.
  cp "${dir}/gateway" "${dir}/gateway.bak"
  if ! install -m 0755 "$RELEASE_BINARY" "${dir}/gateway.new"; then
    die "failed to place new binary — original kept at ${dir}/gateway"
  fi
  mv "${dir}/gateway.new" "${dir}/gateway"
  if ! "${dir}/gateway" version >/dev/null 2>&1; then
    err "new binary failed smoke test — rolling back"
    mv "${dir}/gateway.bak" "${dir}/gateway"
    die "update aborted; previous binary restored"
  fi
  rm -f "${dir}/gateway.bak"
  printf '%s\n' "$version" > "${dir}/VERSION"
  # New release may ship a newer .env.example; never overwrite the live .env.
  cp "${RELEASE_DIR}/.env.example" "${dir}/.env.example" 2>/dev/null || true

  if [ "$was_active" = "1" ]; then
    service_start || die "service failed to start after update — check: journalctl -u ${SERVICE_NAME} -n 50"
    info "${C_G}Updated and running: ${current:-unknown} -> ${version}${C_0}"
  elif [ "$was_running" = "1" ]; then
    (cd "$dir" && nohup ./gateway >> gateway.log 2>&1 &)
    info "${C_G}Updated and restarted via nohup (log: ${dir}/gateway.log): ${version}${C_0}"
  else
    info "${C_G}Updated to ${version}.${C_0} Gateway was not running; start with: cd ${dir} && ./gateway"
  fi
  info "Data and .env were left untouched."
}

# ---------------------------------------------------------------------------
# uninstall
# ---------------------------------------------------------------------------
cmd_uninstall() {
  local dir
  dir="$(find_install_dir)"
  [ -n "$dir" ] || die "no installation found"

  local current confirm_default="n" wipe_default="n"
  [ "${1:-}" = "explicit" ] && confirm_default="y"
  [ "${GATEWAY_PURGE_DATA:-0}" = "1" ] && wipe_default="y"
  current="$(installed_version "$dir" || true)"
  banner
  printf '%b\n' "  Install dir : ${dir}"
  printf '%b\n' "  Version     : ${current:-unknown}"
  hr

  if ! ask_yes_no "Uninstall AI Gateway from ${dir}?" "$confirm_default"; then
    info "Cancelled."
    return
  fi

  info "Stopping gateway..."
  service_stop
  stop_gateway_processes "$dir"

  if [ -f "$SERVICE_FILE" ]; then
    systemctl disable --now "$SERVICE_NAME" >/dev/null 2>&1 || true
    rm -f "$SERVICE_FILE"
    systemctl daemon-reload 2>/dev/null || true
    info "Removed systemd service ${SERVICE_NAME}"
  fi

  rm -f "${dir}/gateway" "${dir}/gateway.bak" "${dir}/gateway.new" "${dir}/VERSION" "${dir}/.env.example" "${dir}/gateway.log"

  if ask_yes_no "Remove ALL data too (SQLite DB, provider keys, .env)? This cannot be undone" "$wipe_default"; then
    rm -rf "$dir"
    if id "$SERVICE_USER" >/dev/null 2>&1 && command -v userdel >/dev/null 2>&1; then
      userdel "$SERVICE_USER" 2>/dev/null || true
    fi
    info "${C_G}Uninstalled. All data removed.${C_0}"
  else
    rmdir "$dir" 2>/dev/null || true
    info "${C_G}Uninstalled binary + service.${C_0}"
    info "Kept configuration & data in ${C_B}${dir}${C_0} (reinstalling there will reuse it)."
  fi
}

# ---------------------------------------------------------------------------
# status
# ---------------------------------------------------------------------------
cmd_status() {
  detect_platform
  local dir current latest
  dir="$(find_install_dir)"
  banner
  if [ -z "$dir" ]; then
    warn "Not installed (checked GATEWAY_INSTALL_DIR, /opt/ai-gateway, ~/.ai-gateway)"
    return
  fi
  current="$(installed_version "$dir" || true)"
  printf '%b\n' "  Install dir : ${dir}"
  printf '%b\n' "  Version     : ${current:-unknown}"
  if service_active; then
    printf '%b\n' "  Service     : ${C_G}running (systemd)${C_0}"
  elif [ -n "$(find_gateway_pids "$dir")" ]; then
    printf '%b\n' "  Process     : ${C_G}running${C_0}"
  else
    printf '%b\n' "  Service     : ${C_Y}not running${C_0}"
  fi
  if [ -d "${dir}/data" ]; then
    printf '%b\n' "  Data        : ${dir}/data ($(du -sh "${dir}/data" 2>/dev/null | awk '{print $1}'))"
  fi
  latest="$(latest_version 2>/dev/null || true)"
  if [ -n "$latest" ]; then
    if [ "$latest" = "$current" ]; then
      printf '%b\n' "  Latest      : ${latest} ${C_G}(up to date)${C_0}"
    else
      printf '%b\n' "  Latest      : ${latest} ${C_Y}(update available — run '$0 update')${C_0}"
    fi
  fi
  hr
}

# ---------------------------------------------------------------------------
# Menu / entrypoint
# ---------------------------------------------------------------------------
usage() {
  cat <<EOF
AI Gateway install manager

  install.sh            interactive menu
  install.sh install    install latest release + .env wizard
  install.sh update     update to latest release (keeps data & .env)
  install.sh uninstall  remove the gateway (asks about data)
  install.sh status     show installed version / service state

Env overrides: GATEWAY_VERSION, GATEWAY_INSTALL_DIR, GATEWAY_REPO, GATEWAY_YES
EOF
}

menu() {
  if [ "$NONINTERACTIVE" = "1" ] || { [ ! -t 0 ] && [ ! -e /dev/tty ]; }; then
    cmd_install # piped without a tty: install with defaults
    return
  fi
  while true; do
    banner
    printf '%b\n' "  ${C_B}1${C_0}) Install    — latest release binary + .env setup"
    printf '%b\n' "  ${C_B}2${C_0}) Update     — pull latest released binary (keeps data)"
    printf '%b\n' "  ${C_B}3${C_0}) Uninstall  — remove gateway (data removal optional)"
    printf '%b\n' "  ${C_B}4${C_0}) Status"
    printf '%b\n' "  ${C_B}q${C_0}) Quit"
    hr
    local choice
    choice="$(ask "Choose an option" "q")"
    echo ""
    case "$choice" in
      1) cmd_install ;;
      2) cmd_update ;;
      3) cmd_uninstall ;;
      4) cmd_status ;;
      q|Q|"") info "Bye."; exit 0 ;;
      *) warn "unknown option: $choice" ;;
    esac
    echo ""
    ask "Press Enter to return to the menu" ""
  done
}

case "${1:-}" in
  ""|menu) menu ;;
  install) cmd_install ;;
  update) cmd_update ;;
  uninstall) cmd_uninstall explicit ;;
  status) cmd_status ;;
  -h|--help|help) usage ;;
  *) err "unknown command: $1"; usage; exit 2 ;;
esac
