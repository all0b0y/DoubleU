"""Functional tests for per-role native-tool enablement."""

from __future__ import annotations

import unittest
from types import SimpleNamespace

from ai_agent.agents.dedup_toolset import DedupToolset
from ai_agent.agents.runtime import resolve_builtin_toolset, role_has_enabled_tools
from ai_agent.agents.tool_catalog import TOOL_CATALOG, TOOL_NAME_TO_KEY, enabled_tool_names
from ai_agent.schemas import AIRoleData, PrivacyFlag


def _is_enabled(toolset: DedupToolset, tool_name: str) -> bool:
    """``resolve_builtin_toolset`` now returns a ``DedupToolset`` wrapping the
    ``FilteredToolset`` — the name filter lives one level down."""
    return toolset.wrapped.filter_func(None, SimpleNamespace(name=tool_name))


class BuiltinToolEnablementTests(unittest.TestCase):
    """Role overrides filter native tools without changing old defaults."""

    def test_missing_settings_preserve_all_catalog_defaults(self) -> None:
        for role in (None, AIRoleData(name="legacy")):
            toolset = resolve_builtin_toolset(role)
            self.assertIsInstance(toolset, DedupToolset)
            for tool_name in TOOL_NAME_TO_KEY:
                key = TOOL_NAME_TO_KEY[tool_name]
                expected = next(
                    entry.default_enabled for entry in TOOL_CATALOG if entry.key == key
                )
                self.assertEqual(_is_enabled(toolset, tool_name), expected)

    def test_disabled_tool_is_absent_from_filtered_toolset(self) -> None:
        role = AIRoleData(
            name="scoped",
            tool_settings={"memory.search_facts": False},
        )
        toolset = resolve_builtin_toolset(role)
        self.assertFalse(_is_enabled(toolset, "search_facts"))
        self.assertTrue(_is_enabled(toolset, "get_recent_messages"))

    def test_interaction_tool_requires_explicit_enablement(self) -> None:
        disabled = resolve_builtin_toolset(AIRoleData(name="legacy"))
        enabled = resolve_builtin_toolset(AIRoleData(
            name="interactive",
            tool_settings={"interaction.ask_questions": True},
        ))
        for tool_name in ("ask_questions", "request_confirmation"):
            self.assertFalse(_is_enabled(disabled, tool_name))
            self.assertTrue(_is_enabled(enabled, tool_name))

    def test_scheduler_list_cancel_are_privacy_gated(self) -> None:
        settings = {entry.key: False for entry in TOOL_CATALOG}
        settings["scheduler.list_reminders"] = True
        settings["scheduler.cancel_reminder"] = True
        role = AIRoleData(name="planner", tool_settings=settings)
        public = resolve_builtin_toolset(role)
        private = resolve_builtin_toolset(role, (PrivacyFlag.NO_MEMORY,))
        for tool_name in ("list_reminders", "cancel_reminder"):
            self.assertTrue(_is_enabled(public, tool_name))
            self.assertFalse(_is_enabled(private, tool_name))

    def test_scheduler_tool_is_privacy_gated(self) -> None:
        settings = {entry.key: False for entry in TOOL_CATALOG}
        settings["scheduler.create_reminder"] = True
        role = AIRoleData(name="scheduler", tool_settings=settings)
        public = resolve_builtin_toolset(role)
        private = resolve_builtin_toolset(role, (PrivacyFlag.PRIVATE,))
        self.assertTrue(_is_enabled(public, "create_reminder"))
        self.assertFalse(_is_enabled(private, "create_reminder"))
        self.assertTrue(role_has_enabled_tools(role))
        self.assertFalse(role_has_enabled_tools(role, (PrivacyFlag.NO_MEMORY,)))

    def test_tool_name_mapping_covers_catalog_once(self) -> None:
        self.assertEqual(set(TOOL_NAME_TO_KEY.values()), {entry.key for entry in TOOL_CATALOG})


class EnabledToolNamesTests(unittest.TestCase):
    """``enabled_tool_names`` is the shared source ``resolve_builtin_toolset``
    and the prompt-policy injection (``prompts/library.py``) both read, so a
    tool's described applicability can never drift from what is actually
    callable this turn."""

    def test_mirrors_resolve_builtin_toolset_for_defaults(self) -> None:
        for role in (None, AIRoleData(name="legacy")):
            names = enabled_tool_names(role)
            toolset = resolve_builtin_toolset(role)
            for tool_name in TOOL_NAME_TO_KEY:
                self.assertEqual(tool_name in names, _is_enabled(toolset, tool_name))

    def test_disabled_tool_is_absent(self) -> None:
        role = AIRoleData(name="scoped", tool_settings={"memory.search_facts": False})
        names = enabled_tool_names(role)
        self.assertNotIn("search_facts", names)
        self.assertIn("get_recent_messages", names)

    def test_schedule_category_blocked_under_no_memory(self) -> None:
        settings = {entry.key: False for entry in TOOL_CATALOG}
        settings["scheduler.create_reminder"] = True
        role = AIRoleData(name="scheduler", tool_settings=settings)
        self.assertIn("create_reminder", enabled_tool_names(role))
        self.assertNotIn("create_reminder", enabled_tool_names(role, (PrivacyFlag.NO_MEMORY,)))


if __name__ == "__main__":
    unittest.main()
