#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO_DIR="${GO_DIR:-${ROOT_DIR}/backend-go}"
THRESHOLD="${GO_COVERAGE_THRESHOLD:-70}"
COVERAGE_DIR="${ROOT_DIR}/.qa_coverage/go"
GO_CACHE_DIR="${ROOT_DIR}/.qa_coverage/go-build"

if [[ ! -d "${GO_DIR}" ]]; then
  echo "Go coverage check failed: ${GO_DIR} does not exist yet." >&2
  exit 1
fi

if [[ ! -f "${GO_DIR}/go.mod" ]]; then
  echo "Go coverage check failed: ${GO_DIR}/go.mod is missing." >&2
  exit 1
fi

cd "${GO_DIR}"

mkdir -p "${COVERAGE_DIR}" "${GO_CACHE_DIR}"

GOCACHE="${GO_CACHE_DIR}" go test ./... -covermode=atomic -coverprofile="${COVERAGE_DIR}/coverage.out"

coverage_line="$(GOCACHE="${GO_CACHE_DIR}" go tool cover -func="${COVERAGE_DIR}/coverage.out" | awk '/^total:/ {print $3}')"
coverage_value="${coverage_line%%%}"

python3 - "$coverage_value" "$THRESHOLD" <<'PY'
import sys

actual = float(sys.argv[1])
threshold = float(sys.argv[2])

if actual < threshold:
    print(f"Go coverage check failed: {actual:.1f}% < {threshold:.1f}%", file=sys.stderr)
    sys.exit(1)

print(f"Go coverage check passed: {actual:.1f}% >= {threshold:.1f}%")
PY
