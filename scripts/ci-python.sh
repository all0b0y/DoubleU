#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

PYTHON_BIN="${PYTHON_BIN:-python3}"

if [[ ! -d ai-python ]]; then
  echo "ai-python directory not found; skipping Python tests."
  exit 0
fi

export PYTHONPATH="$ROOT_DIR/ai-python${PYTHONPATH:+:$PYTHONPATH}"

if find ai-python/tests -name '*.py' 2>/dev/null | grep -q .; then
  "$PYTHON_BIN" -m unittest discover -s ai-python/tests -p 'test*.py'
elif find ai-python -name 'test*.py' | grep -q .; then
  "$PYTHON_BIN" -m unittest discover -s ai-python -p 'test*.py'
else
  echo "No Python tests found; compiling Python sources if present."
  if find ai-python -name '*.py' | grep -q .; then
    "$PYTHON_BIN" -m compileall -q ai-python
  fi
fi

if [[ -f scripts/check_policy_matrix.py ]]; then
  "$PYTHON_BIN" scripts/check_policy_matrix.py >/dev/null
fi
