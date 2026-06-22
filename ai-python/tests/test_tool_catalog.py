"""Functional tests for per-role native-tool enablement."""

from __future__ import annotations

import unittest
from types import SimpleNamespace

from pydantic_ai.toolsets import FilteredToolset

from ai_agent.agents.runtime import resolve_builtin_toolset
from ai_agent.agents.tool_catalog import TOOL_CATALOG, TOOL_NAME_TO_KEY
from ai_agent.schemas import AIRoleData


class BuiltinToolEnablementTests(unittest.TestCase):
    """Role overrides filter native tools without changing old defaults."""

    def test_missing_settings_preserve_all_catalog_defaults(self) -> None:
        for role in (None, AIRoleData(name="legacy")):
            toolset = resolve_builtin_toolset(role)
            self.assertIsInstance(toolset, FilteredToolset)
            for tool_name in TOOL_NAME_TO_KEY:
                self.assertTrue(toolset.filter_func(None, SimpleNamespace(name=tool_name)))

    def test_disabled_tool_is_absent_from_filtered_toolset(self) -> None:
        role = AIRoleData(
            name="scoped",
            tool_settings={"memory.search_facts": False},
        )
        toolset = resolve_builtin_toolset(role)
        self.assertFalse(toolset.filter_func(None, SimpleNamespace(name="search_facts")))
        self.assertTrue(toolset.filter_func(None, SimpleNamespace(name="get_recent_messages")))

    def test_tool_name_mapping_covers_catalog_once(self) -> None:
        self.assertEqual(set(TOOL_NAME_TO_KEY.values()), {entry.key for entry in TOOL_CATALOG})


if __name__ == "__main__":
    unittest.main()
