#!/usr/bin/env bash
# Verifies that SoroBeacon release binaries (`sorobeacon` and
# `terraform-provider-sorobeacon`) are byte-for-byte reproducible when built
# from the same commit in two deliberately different environments:
#   - Different working directory paths and directory depths
#   - Different HOME, GOPATH, GOCACHE, TMPDIR
#   - Different TZ (UTC vs America/New_York) and LC_ALL
#   - Different HOSTNAME / USER environment variables
#
# Also verifies that `/api/v1/version` (`internal/buildinfo`) metadata
# (Version, Commit, Date derived from the commit timestamp) is preserved in the
# binary rather than blanked out, and prints an actionable section/symbol/byte
# diff if the two builds ever diverge.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/sorobeacon-repro.XXXXXX")"
trap 'rm -rf "${WORK_ROOT}"' EXIT

# Derive deterministic build metadata from the git commit (or SOURCE_DATE_EPOCH).
VERSION="${VERSION:-$(git -C "${REPO_ROOT}" describe --tags --always --dirty 2>/dev/null || echo v0.0.0-repro)}"
COMMIT="${COMMIT:-$(git -C "${REPO_ROOT}" rev-parse --short=12 HEAD 2>/dev/null || echo 000000000000)}"
if [[ -n "${SOURCE_DATE_EPOCH:-}" ]]; then
  DATE="$(TZ=UTC date -d "@${SOURCE_DATE_EPOCH}" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || TZ=UTC date -r "${SOURCE_DATE_EPOCH}" +%Y-%m-%dT%H:%M:%SZ)"
else
  SOURCE_DATE_EPOCH="$(git -C "${REPO_ROOT}" show -s --format=%ct HEAD)"
  DATE="$(TZ=UTC git -C "${REPO_ROOT}" show -s --format=%cd --date=format-local:%Y-%m-%dT%H:%M:%SZ HEAD)"
fi

if [[ -z "${VERSION}" || -z "${COMMIT}" || -z "${DATE}" || "${DATE}" == "unknown" ]]; then
  echo "::error::Build metadata must be non-empty (VERSION=${VERSION}, COMMIT=${COMMIT}, DATE=${DATE})" >&2
  exit 1
fi

echo "==> Reproducible build verification"
echo "    Toolchain : $(go version)"
echo "    VERSION   : ${VERSION}"
echo "    COMMIT    : ${COMMIT}"
echo "    DATE      : ${DATE} (SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH})"

SRC_A="${WORK_ROOT}/env-a/src"
SRC_B="${WORK_ROOT}/env-b/deeply/nested/build/path/sorobeacon-checkout"
OUT_A="${WORK_ROOT}/out-a"
OUT_B="${WORK_ROOT}/out-b"
mkdir -p "${SRC_A}" "${SRC_B}" "${OUT_A}" "${OUT_B}"

# Copy tracked source tree into two distinct directory hierarchies without .git
# so VCS state or path depth cannot leak into the binary.
git -C "${REPO_ROOT}" archive HEAD | tar -x -C "${SRC_A}"
git -C "${REPO_ROOT}" archive HEAD | tar -x -C "${SRC_B}"

LDFLAGS="-s -w -buildid= -X github.com/sorotrail/sorobeacon/internal/buildinfo.Version=${VERSION} -X github.com/sorotrail/sorobeacon/internal/buildinfo.Commit=${COMMIT} -X github.com/sorotrail/sorobeacon/internal/buildinfo.Date=${DATE}"
TF_LDFLAGS="-s -w -buildid="

build_in_env() {
  local label="$1"
  local src_dir="$2"
  local out_dir="$3"
  local home_dir="$4"
  local tz_val="$5"
  local host_val="$6"
  local user_val="$7"

  mkdir -p "${home_dir}/gopath" "${home_dir}/gocache" "${home_dir}/tmp"
  echo "==> Building (${label}) in ${src_dir} (TZ=${tz_val}, HOSTNAME=${host_val})"

  (
    cd "${src_dir}"
    env \
      HOME="${home_dir}" \
      USER="${user_val}" \
      HOSTNAME="${host_val}" \
      TZ="${tz_val}" \
      LC_ALL=C \
      TMPDIR="${home_dir}/tmp" \
      GOCACHE="${home_dir}/gocache" \
      CGO_ENABLED=0 \
      GOOS="${GOOS:-linux}" \
      GOARCH="${GOARCH:-amd64}" \
      SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH}" \
      go build -trimpath -buildvcs=false -ldflags "${LDFLAGS}" -o "${out_dir}/sorobeacon" ./cmd/sorobeacon

    cd "${src_dir}/terraform-provider-sorobeacon"
    env \
      HOME="${home_dir}" \
      USER="${user_val}" \
      HOSTNAME="${host_val}" \
      TZ="${tz_val}" \
      LC_ALL=C \
      TMPDIR="${home_dir}/tmp" \
      GOCACHE="${home_dir}/gocache" \
      CGO_ENABLED=0 \
      GOOS="${GOOS:-linux}" \
      GOARCH="${GOARCH:-amd64}" \
      SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH}" \
      go build -trimpath -buildvcs=false -ldflags "${TF_LDFLAGS}" -o "${out_dir}/terraform-provider-sorobeacon" .
  )
}

diagnose_binary_diff() {
  local name="$1"
  local bin_a="$2"
  local bin_b="$3"

  echo "::error::Reproducibility check FAILED for ${name}: binaries are not byte-for-byte identical!" >&2
  echo "=== [1/4] Size & SHA-256 comparison (${name}) ===" >&2
  ls -la "${bin_a}" "${bin_b}" >&2 || true
  sha256sum "${bin_a}" "${bin_b}" >&2 || true

  echo "=== [2/4] Go build metadata diff (go version -m) ===" >&2
  diff -u <(go version -m "${bin_a}" 2>&1 || true) <(go version -m "${bin_b}" 2>&1 || true) >&2 || true

  if command -v objdump >/dev/null 2>&1; then
    echo "=== [3/4] Section headers diff (objdump -h) ===" >&2
    diff -u <(objdump -h "${bin_a}" 2>&1 | sed 's|'"${bin_a}"'|BINARY|g' || true) \
            <(objdump -h "${bin_b}" 2>&1 | sed 's|'"${bin_b}"'|BINARY|g' || true) >&2 || true
  fi

  echo "=== [4/4] First 40 differing byte offsets (cmp -l) ===" >&2
  cmp -l "${bin_a}" "${bin_b}" 2>&1 | head -n 40 >&2 || true
}

build_in_env "env-alpha" "${SRC_A}" "${OUT_A}" "${WORK_ROOT}/home-a" "UTC" "builder-alpha.internal" "alice"
build_in_env "env-bravo" "${SRC_B}" "${OUT_B}" "${WORK_ROOT}/home-b" "America/New_York" "builder-bravo.example.org" "bob"

# Verify that buildinfo values are actually stamped into the compiled binary
# (reproducibility must not come from erasing Version/Commit/Date).
for expected in "${VERSION}" "${COMMIT}" "${DATE}"; do
  if ! grep -aFq "${expected}" "${OUT_A}/sorobeacon"; then
    echo "::error::Expected buildinfo value '${expected}' was not found in ${OUT_A}/sorobeacon" >&2
    exit 1
  fi
done
echo "==> Verified stamped buildinfo in binary (VERSION=${VERSION}, COMMIT=${COMMIT}, DATE=${DATE})"

# Verify that build paths did not leak into the binary.
if grep -aFq "${WORK_ROOT}" "${OUT_A}/sorobeacon" || grep -aFq "${WORK_ROOT}" "${OUT_B}/sorobeacon"; then
  echo "::error::Build directory path (${WORK_ROOT}) leaked into compiled binary!" >&2
  exit 1
fi

FAILED=0
for bin_name in sorobeacon terraform-provider-sorobeacon; do
  sha_a="$(sha256sum "${OUT_A}/${bin_name}" | awk '{print $1}')"
  sha_b="$(sha256sum "${OUT_B}/${bin_name}" | awk '{print $1}')"
  echo "    ${bin_name}:"
  echo "      env-alpha SHA-256: ${sha_a}"
  echo "      env-bravo SHA-256: ${sha_b}"
  if [[ "${sha_a}" != "${sha_b}" ]]; then
    diagnose_binary_diff "${bin_name}" "${OUT_A}/${bin_name}" "${OUT_B}/${bin_name}"
    FAILED=1
  fi
done

if [[ "${FAILED}" -ne 0 ]]; then
  exit 1
fi

echo "==> SUCCESS: Both sorobeacon and terraform-provider-sorobeacon are 100% byte-for-byte reproducible."
