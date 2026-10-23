#!/usr/bin/env bash
set -euo pipefail

version=${1:-0.2.6}
[[ ${version} =~ ^[0-9]+\.[0-9]+\.[0-9]+([.-][A-Za-z0-9.-]+)?$ ]] || {
  echo "Usage: ./deploy/build-centos-package.sh [semantic-version]" >&2
  exit 2
}

repository=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
stage=$(mktemp -d /tmp/codex-relay-package.XXXXXX)
package_name=codex-relay-centos-linux-amd64-${version}
package_root=${stage}/${package_name}
output=${repository}/dist/${package_name}.tar.gz

cleanup() {
  rm -rf -- "${stage}"
}
trap cleanup EXIT

mkdir -p "${package_root}" "${repository}/dist"
commit=${RELEASE_SOURCE_COMMIT:-}
if [[ -z ${commit} ]]; then
  commit=$(git -C "${repository}" rev-parse --short=12 HEAD)
  if [[ -n $(git -C "${repository}" status --porcelain) ]]; then commit=${commit}+dirty; fi
fi
build_date=$(date -u +%Y-%m-%dT%H:%M:%SZ)

(
  cd "${repository}"
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${version} -X main.commit=${commit} -X main.date=${build_date}" \
    -o "${package_root}/codex-relay" \
    ./cmd/codex-relay
)

cp "${repository}/deploy/install-centos.sh" "${package_root}/install.sh"
cp "${repository}/deploy/codex-relay.service" "${package_root}/codex-relay.service"
cp "${repository}/deploy/relay.reverse-proxy.example.yaml" "${package_root}/relay.yaml"
cp "${repository}/deploy/nginx.conf.example" "${package_root}/nginx.conf.example"
cp "${repository}/README.md" "${repository}/DEPLOYMENT.md" "${repository}/DEVICE-CLAIM.md" "${repository}/RELEASE-ACCEPTANCE.md" "${package_root}/"
chmod 0755 "${package_root}/codex-relay" "${package_root}/install.sh"

COPYFILE_DISABLE=1 tar -czf "${output}" -C "${stage}" "${package_name}"
checksum=$(shasum -a 256 "${output}" | awk '{print $1}')
printf '%s  %s\n' "${checksum}" "$(basename "${output}")" > "${output}.sha256"
printf '{"output":"%s","sha256":"%s"}\n' "${output}" "${checksum}"
