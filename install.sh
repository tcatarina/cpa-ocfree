#!/usr/bin/env bash
# Install or upgrade the ocfree CPA plugin and wire it into config.yaml.
#
#   ./install.sh              install or upgrade, then print what to do next
#   ./install.sh --uninstall  remove the plugin and its config block
#
# The management key is read from the existing .env next to config.yaml, so it
# never has to be pasted by hand.
set -euo pipefail

ROOT="${CPA_ROOT:-/home/ubuntu/docker/cliproxyapi}"
PLUGIN_ID="ocfree"
ARCH="${CPA_ARCH:-$(uname -m)}"
case "${ARCH}" in
  x86_64)  TARGET="linux-amd64" ;;
  aarch64) TARGET="linux-arm64" ;;
  *) echo "unsupported architecture: ${ARCH}" >&2; exit 1 ;;
esac

ENV_FILE="${ROOT}/.env"
CONFIG="${ROOT}/config.yaml"
PLUGIN_DIR="${ROOT}/plugins/linux/${ARCH}"
BASE_URL="${CPA_BASE_URL:-http://127.0.0.1:8317}"
REPO="tcatarina/cpa-ocfree"

log() { printf '  %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

read_management_key() {
  [ -f "${ENV_FILE}" ] || die "no .env at ${ENV_FILE}"
  set -a; . "${ENV_FILE}"; set +a
  [ -n "${CPA_MANAGEMENT_KEY:-}" ] || die "CPA_MANAGEMENT_KEY is not set in ${ENV_FILE}"
  printf '%s' "${CPA_MANAGEMENT_KEY}"
}

latest_tag() {
  curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
    | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1
}

fetch_plugin_block() {
  curl -fsSL -H "Authorization: Bearer ${1}" \
    "${BASE_URL}/v0/resource/plugins/${PLUGIN_ID}/${PLUGIN_ID}?asset=provider"
}

if [ "${1:-}" = "--uninstall" ]; then
  log "removing ${PLUGIN_DIR}/${PLUGIN_ID}.so"
  sudo rm -f "${PLUGIN_DIR}/${PLUGIN_ID}.so"
  log "stop the container and drop the '${PLUGIN_ID}' block from ${CONFIG} by hand"
  exit 0
fi

[ -d "${PLUGIN_DIR}" ] || die "no plugin directory at ${PLUGIN_DIR}"
[ -f "${CONFIG}" ] || die "no config at ${CONFIG}"

TAG="${CPA_VERSION:-$(latest_tag)}"
[ -n "${TAG}" ] || die "could not determine the latest release"
log "installing ${REPO} ${TAG} for ${TARGET}"

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT
curl -fsSL -o "${TMP}/p.tar.gz" \
  "https://github.com/${REPO}/releases/download/${TAG}/${PLUGIN_ID}_${TAG#v}_${TARGET}.tar.gz"
tar -xzf "${TMP}/p.tar.gz" -C "${TMP}"

sudo install -m 600 -o root -g root \
  "${TMP}/${PLUGIN_ID}_${TAG#v}_${TARGET}/${PLUGIN_ID}.so" "${PLUGIN_DIR}/${PLUGIN_ID}.so"
log "installed ${PLUGIN_DIR}/${PLUGIN_ID}.so"

# The provider block is served by the plugin's own management route, so on a
# first install it does not exist yet. Enable the plugin and restart once to load
# the binary, then read the block, then restart again for the config change.
restart() {
  [ -n "${CPA_CONTAINER:-}" ] || return 0
  log "restarting ${CPA_CONTAINER}"
  docker restart "${CPA_CONTAINER}" >/dev/null
  sleep "${CPA_RESTART_WAIT:-14}"
}

if grep -q "name: opencode-free" "${CONFIG}"; then
  log "provider block already present in ${CONFIG}"
  restart
else
  if ! grep -qE "^\s+${PLUGIN_ID}:" "${CONFIG}"; then
    die "add this under plugins: -> configs: in ${CONFIG}, then run again:
    ${PLUGIN_ID}:
      enabled: true"
  fi
  log "restarting to load the plugin before reading its provider block"
  restart
  BLOCK="$(fetch_plugin_block "$(read_management_key)")"
  [ -n "${BLOCK}" ] || die "could not read the provider block from the running panel"
  printf '\n%s\n' "${BLOCK}" | sudo tee -a "${CONFIG}" >/dev/null
  log "appended the provider block to ${CONFIG}"
  log "restarting to pick up the config change"
  restart
fi

cat <<NEXT

  done. check the models arrived:

      curl -s -H "Authorization: Bearer <your api key>" ${BASE_URL}/v1/models | grep space-bunny

  if you set CPA_CONTAINER the container was restarted for you; otherwise restart it
  yourself now.

NEXT
