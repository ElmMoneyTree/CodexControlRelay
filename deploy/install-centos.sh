#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "Usage: sudo ./install.sh https://relay.example.com" >&2
  exit 2
}

if [[ ${EUID} -ne 0 ]]; then
  echo "Please run this installer as root (sudo)." >&2
  exit 1
fi

public_url=${1:-}
[[ $# -eq 1 ]] || usage
[[ ${public_url} =~ ^https://[A-Za-z0-9.-]+(:[0-9]+)?$ ]] || {
  echo "Public URL must be an HTTPS origin without a path, query, or fragment." >&2
  usage
}

package_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
binary=${package_dir}/codex-relay
config_template=${package_dir}/relay.yaml
service_template=${package_dir}/codex-relay.service
installed_binary=/opt/codex-relay/codex-relay
staged_binary=/opt/codex-relay/.codex-relay-install-$$

cleanup() {
  rm -f -- "${staged_binary}"
}
trap cleanup EXIT

[[ -x ${binary} ]] || { echo "Package is missing executable codex-relay." >&2; exit 1; }
[[ -f ${config_template} ]] || { echo "Package is missing relay.yaml." >&2; exit 1; }
[[ -f ${service_template} ]] || { echo "Package is missing codex-relay.service." >&2; exit 1; }
command -v systemctl >/dev/null || { echo "systemd is required." >&2; exit 1; }
command -v openssl >/dev/null || { echo "openssl is required to generate the control token." >&2; exit 1; }

getent group codex-relay >/dev/null 2>&1 || groupadd --system codex-relay
id codex-relay >/dev/null 2>&1 || useradd \
  --system \
  --gid codex-relay \
  --home-dir /var/lib/codex-relay \
  --shell /sbin/nologin \
  codex-relay

install -d -m 0755 -o root -g root /opt/codex-relay
install -d -m 0750 -o root -g codex-relay /etc/codex-relay
install -d -m 0750 -o codex-relay -g codex-relay /var/lib/codex-relay

# Stage and atomically replace the executable. A running old process can keep
# its inode until the explicit systemd restart below.
install -m 0755 -o root -g root "${binary}" "${staged_binary}"
mv -f -- "${staged_binary}" "${installed_binary}"

if [[ ! -f /etc/codex-relay/relay.yaml ]]; then
  install -m 0640 -o root -g codex-relay "${config_template}" /etc/codex-relay/relay.yaml
  escaped_url=${public_url//&/\\&}
  sed -i "s|https://relay.example.com|${escaped_url}|" /etc/codex-relay/relay.yaml
else
  echo "Keeping existing /etc/codex-relay/relay.yaml"
fi

if [[ ! -f /etc/codex-relay/relay.env ]]; then
  control_token=$(openssl rand -hex 32)
  umask 077
  printf 'CODEX_RELAY_CONTROL_TOKEN=%s\n' "${control_token}" > /etc/codex-relay/relay.env
  chown root:codex-relay /etc/codex-relay/relay.env
  chmod 0640 /etc/codex-relay/relay.env
  unset control_token
else
  echo "Keeping existing /etc/codex-relay/relay.env"
fi

install -m 0644 -o root -g root "${service_template}" /etc/systemd/system/codex-relay.service

set -a
# shellcheck disable=SC1091
source /etc/codex-relay/relay.env
set +a
"${installed_binary}" --config /etc/codex-relay/relay.yaml --check-config
unset CODEX_RELAY_CONTROL_TOKEN

systemctl daemon-reload
systemctl enable codex-relay >/dev/null
# enable --now does not restart an already-running service after an upgrade.
systemctl restart codex-relay

if command -v curl >/dev/null; then
  ready=false
  for _ in {1..15}; do
    if curl -fsS --max-time 2 http://127.0.0.1:8787/ready >/dev/null; then
      ready=true
      break
    fi
    sleep 1
  done
  if [[ ${ready} != true ]]; then
    echo "Codex Relay did not become ready. Recent service logs:" >&2
    journalctl -u codex-relay -n 50 --no-pager >&2 || true
    exit 1
  fi
else
  systemctl is-active --quiet codex-relay
fi

echo
echo "Codex Relay is installed, restarted, and ready."
echo "Public URL: ${public_url}"
echo "Local health check: curl -fsS http://127.0.0.1:8787/ready"
echo "View status: systemctl status codex-relay --no-pager"
echo "The Controller token is stored in /etc/codex-relay/relay.env"
echo "Configure Nginx or Caddy TLS before connecting from a phone or Agent."
