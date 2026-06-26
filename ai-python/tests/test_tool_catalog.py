"""Functional tests for per-role native-tool enablement."""

from __future__ import annotations

import unittest
from types import SimpleNamespace

from pydantic_ai.toolsets import FilteredToolset

from ai_agent.agents.runtime import resolve_builtin_toolset, role_has_enabled_tools
from ai_agent.agents.tool_catalog import TOOL_CATALOG, TOOL_NAME_TO_KEY
from ai_agent.schemas import AIRoleData, PrivacyFlag


class BuiltinToolEnablementTests(unittest.TestCase):
    """Role overrides filter native tools without changing old defaults."""

    def test_missing_settings_preserve_all_catalog_defaults(self) -> None:
        for role in (None, AIRoleData(name="legacy")):
            toolset = resolve_builtin_toolset(role)
            self.assertIsInstance(toolset, FilteredToolset)
            for tool_name in TOOL_NAME_TO_KEY:
                key = TOOL_NAME_TO_KEY[tool_name]
                expected = next(
                    entry.default_enabled for entry in TOOL_CATALOG if entry.key == key
                )
                self.assertEqual(
                    toolset.filter_func(None, SimpleNamespace(name=tool_name)), expected
                )

    def test_disabled_tool_is_absent_from_filtered_toolset(self) -> None:
        role = AIRoleData(
            name="scoped",
            tool_settings={"memory.search_facts": False},
        )
        toolset = resolve_builtin_toolset(role)
        self.assertFalse(toolset.filter_func(None, SimpleNamespace(name="search_facts")))
        self.assertTrue(toolset.filter_func(None, SimpleNamespace(name="get_recent_messages")))

    def test_interaction_tool_requires_explicit_enablement(self) -> None:
        disabled = resolve_builtin_toolset(AIRoleData(name="legacy"))
        enabled = resolve_builtin_toolset(AIRoleData(
            name="interactive",
            tool_settings={"interaction.present_buttons": True},
        ))
        for tool_name in ("present_buttons", "request_confirmation"):
            self.assertFalse(disabled.filter_func(None, SimpleNamespace(name=tool_name)))
            self.assertTrue(enabled.filter_func(None, SimpleNamespace(name=tool_name)))

    def test_scheduler_tool_is_privacy_gated(self) -> None:
        settings = {entry.key: False for entry in TOOL_CATALOG}
        settings["scheduler.create_reminder"] = True
        role = AIRoleData(name="scheduler", tool_settings=settings)
        public = resolve_builtin_toolset(role)
        private = resolve_builtin_toolset(role, (PrivacyFlag.PRIVATE,))
        tool = SimpleNamespace(name="create_reminder")
        self.assertTrue(public.filter_func(None, tool))
        self.assertFalse(private.filter_func(None, tool))
        self.assertTrue(role_has_enabled_tools(role))
        self.assertFalse(role_has_enabled_tools(role, (PrivacyFlag.NO_MEMORY,)))

    def test_tool_name_mapping_covers_catalog_once(self) -> None:
        self.assertEqual(set(TOOL_NAME_TO_KEY.values()), {entry.key for entry in TOOL_CATALOG})


if __name__ == "__main__":
    unittest.main()
