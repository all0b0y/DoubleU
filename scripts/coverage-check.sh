#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MIN_COVERAGE="${MIN_COVERAGE:-70}"
PYTHON_BIN="${PYTHON_BIN:-python3}"

if [[ -f "$ROOT_DIR/coverage-go.out" && -f "$ROOT_DIR/backend-go/go.mod" ]]; then
  mkdir -p "$ROOT_DIR/.qa_coverage/go-build"
  total="$(cd "$ROOT_DIR/backend-go" && GOCACHE="$ROOT_DIR/.qa_coverage/go-build" go tool cover -func="$ROOT_DIR/coverage-go.out" | awk '/^total:/ {gsub("%", "", $3); print $3}')"
  python3 - "$total" "$MIN_COVERAGE" <<'PY'
import sys
actual = float(sys.argv[1])
minimum = float(sys.argv[2])
if actual < minimum:
    raise SystemExit(f"Go coverage {actual:.1f}% is below required {minimum:.1f}%")
print(f"Go coverage {actual:.1f}% meets required {minimum:.1f}%")
PY
else
  echo "No Go coverage profile found; skipping Go coverage check."
fi

if [[ -d "$ROOT_DIR/ai-python/ai_agent" ]] && find "$ROOT_DIR/ai-python/tests" -name '*.py' 2>/dev/null | grep -q .; then
  TRACE_DIR="$ROOT_DIR/.qa_coverage/python-trace"
  rm -rf "$TRACE_DIR"
  mkdir -p "$TRACE_DIR"
  export PYTHONPATH="$ROOT_DIR/ai-python${PYTHONPATH:+:$PYTHONPATH}"
  "$PYTHON_BIN" -m trace --count --coverdir "$TRACE_DIR" "$ROOT_DIR/scripts/run_python_unittest.py" >/dev/null
  "$PYTHON_BIN" - "$ROOT_DIR" "$TRACE_DIR" "$MIN_COVERAGE" <<'PY'
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
    raise SystemExit(f"Python trace coverage {actual:.1f}% is below required {minimum:.1f}%")
print(f"Python trace coverage {actual:.1f}% meets required {minimum:.1f}%")
PY
else
  echo "No Python sources/tests found; skipping Python coverage check."
fi

echo "Coverage checks complete."
