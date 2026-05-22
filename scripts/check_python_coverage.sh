#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PYTHON_BIN="${PYTHON_BIN:-python3}"
THRESHOLD="${PY_COVERAGE_THRESHOLD:-70}"
TRACE_DIR="${ROOT_DIR}/.qa_coverage/python-trace"

if [[ ! -d "${ROOT_DIR}/ai-python/ai_agent" ]]; then
  echo "Python coverage check failed: ai-python/ai_agent is missing." >&2
  exit 1
fi

rm -rf "${TRACE_DIR}"
mkdir -p "${TRACE_DIR}"
export PYTHONPATH="${ROOT_DIR}/ai-python${PYTHONPATH:+:${PYTHONPATH}}"

"${PYTHON_BIN}" -m trace --count --coverdir "${TRACE_DIR}" "${ROOT_DIR}/scripts/run_python_unittest.py" >/dev/null

"${PYTHON_BIN}" - "${ROOT_DIR}" "${TRACE_DIR}" "${THRESHOLD}" <<'PY'
from pathlib import Path
import sys

root = Path(sys.argv[1])
trace_dir = Path(sys.argv[2])
minimum = float(sys.argv[3])
source_files = sorted((root / "ai-python" / "ai_agent").glob("*.py"))
total = 0
covered = 0

for source in source_files:
    trace_name = ".".join(source.relative_to(root / "ai-python").with_suffix("").parts) + ".cover"
    cover_file = trace_dir / trace_name
    if not cover_file.exists():
        continue
    for raw in cover_file.read_text().splitlines():
        stripped = raw.strip()
        if not stripped or stripped.startswith(("#", '"""', "'''")):
            continue
        if raw[:6].strip() == "":
            total += 1
            continue
        prefix = raw[:6].strip().rstrip(":")
        if prefix.isdigit():
            total += 1
            covered += 1

actual = 100.0 if total == 0 else covered * 100.0 / total
if actual < minimum:
    raise SystemExit(f"Python coverage check failed: {actual:.1f}% < {minimum:.1f}%")
print(f"Python coverage check passed: {actual:.1f}% >= {minimum:.1f}%")
PY
