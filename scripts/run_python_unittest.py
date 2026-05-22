#!/usr/bin/env python3
"""Run Python unittest discovery for coverage tooling."""

from __future__ import annotations

import pathlib
import sys
import unittest


ROOT_DIR = pathlib.Path(__file__).resolve().parents[1]
AI_DIR = ROOT_DIR / "ai-python"
TEST_DIR = AI_DIR / "tests"

sys.path.insert(0, str(AI_DIR))

suite = unittest.defaultTestLoader.discover(str(TEST_DIR), pattern="test*.py")
result = unittest.TextTestRunner(verbosity=1).run(suite)
raise SystemExit(0 if result.wasSuccessful() else 1)
