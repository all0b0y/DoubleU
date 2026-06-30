"""Coverage and content tests for :mod:`ai_agent.agents.tool_policy`.

``TOOL_PROMPT_POLICIES`` is the structured layer the orchestrator/writer
prompts draw applicability from (see ``test_prompt_library.py`` for the
rendering side) — these tests only check the registry and its renderers in
isolation: full coverage of every native tool, and that the three read-tool
rules the plan calls out explicitly (``get_contact_profile``,
``get_recent_messages``, ``search_facts``) are actually present.
"""

from __future__ import annotations

from ai_agent.agents.tool_catalog import TOOL_NAME_TO_KEY
from ai_agent.agents.tool_policy import (
    TOOL_PROMPT_POLICIES,
    ToolPromptPolicy,
    applicable_policy_text,
    render_policy,
)

# send_message has no TOOL_CATALOG entry (always-on writer tool, never
# user-toggleable) but is still a native tool the orchestrator/writer can
# call, so it belongs in the policy registry alongside the catalog tools.
_ALL_NATIVE_TOOL_NAMES = frozenset(TOOL_NAME_TO_KEY) | {"send_message"}


def test_every_native_tool_has_a_policy() -> None:
    missing = _ALL_NATIVE_TOOL_NAMES - set(TOOL_PROMPT_POLICIES)
    assert not missing, f"no ToolPromptPolicy for: {sorted(missing)}"


def test_no_orphan_policy_entries() -> None:
    """Every registry key must name a real tool — catches typos/renames."""
    orphans = set(TOOL_PROMPT_POLICIES) - _ALL_NATIVE_TOOL_NAMES
    assert not orphans, f"ToolPromptPolicy entries for non-existent tools: {sorted(orphans)}"


def test_every_policy_declares_purpose_and_applicability() -> None:
    """purpose/use_when/do_not_use_when are the fields a model actually reads
    (purpose+use_when in the docstring, use_when/do_not_use_when in the
    rendered orchestrator/writer prompt) — none may be silently empty."""
    for name, policy in TOOL_PROMPT_POLICIES.items():
        assert policy.tool_name == name, f"{name}: tool_name field doesn't match its registry key"
        assert policy.purpose.strip(), f"{name}: empty purpose"
        assert policy.use_when, f"{name}: empty use_when"
        assert policy.do_not_use_when, f"{name}: empty do_not_use_when"


def test_side_effect_is_a_known_value() -> None:
    allowed = {"none", "staged_memory_write", "staged_schedule", "staged_interaction", "staged_delivery"}
    for name, policy in TOOL_PROMPT_POLICIES.items():
        assert policy.side_effect in allowed, f"{name}: unknown side_effect {policy.side_effect!r}"


class TestExplicitlyRequiredReadToolRules:
    """The plan calls these three out by name — lock the exact guidance in."""

    def test_get_contact_profile_skips_when_profile_already_in_context(self) -> None:
        policy = TOOL_PROMPT_POLICIES["get_contact_profile"]
        assert any("trusted" in line or "контекст" in line for line in policy.do_not_use_when)

    def test_get_recent_messages_skips_first_message_with_enough_context(self) -> None:
        policy = TOOL_PROMPT_POLICIES["get_recent_messages"]
        assert any("первое сообщение" in line for line in policy.do_not_use_when)

    def test_search_facts_is_this_chat_only_not_general_knowledge(self) -> None:
        policy = TOOL_PROMPT_POLICIES["search_facts"]
        assert any("общие знания" in line for line in policy.do_not_use_when)


def test_render_policy_includes_name_purpose_and_both_applicability_clauses() -> None:
    policy = ToolPromptPolicy(
        tool_name="example_tool",
        purpose="Делает пример.",
        use_when=("условие A.",),
        do_not_use_when=("условие B.",),
    )
    text = render_policy(policy)
    assert text.startswith("example_tool: Делает пример.")
    assert "Используй когда: условие A." in text
    assert "НЕ используй когда: условие B." in text


def test_render_policy_omits_empty_clauses() -> None:
    policy = ToolPromptPolicy(tool_name="bare", purpose="Минимум.", use_when=(), do_not_use_when=())
    text = render_policy(policy)
    assert text == "bare: Минимум."


def test_applicable_policy_text_only_covers_requested_names_in_sorted_order() -> None:
    lines = applicable_policy_text(frozenset({"search_facts", "create_reminder"}))
    assert len(lines) == 2
    # Sorted alphabetically: "create_reminder" < "search_facts".
    assert lines[0].startswith("create_reminder:")
    assert lines[1].startswith("search_facts:")


def test_applicable_policy_text_skips_unknown_names_without_raising() -> None:
    lines = applicable_policy_text(frozenset({"search_facts", "not_a_real_tool"}))
    assert len(lines) == 1
    assert lines[0].startswith("search_facts:")


def test_applicable_policy_text_empty_input_yields_empty_output() -> None:
    assert applicable_policy_text(frozenset()) == ()
