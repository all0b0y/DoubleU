# DoubleU — Project Charter for AI Agents

This file defines the mandatory rules for all AI agents (Claude Code and similar) working on this codebase.
**All rules are binding. Deviating from them requires explicit user instruction.**

---

## Code Quality

- Write **clean, readable code**. Prefer clarity over cleverness.
- Write **algorithmically efficient code**. Avoid unnecessary loops, allocations, and DB calls.
- **No boilerplate** that only works for one specific case. Code must handle the general case.
- **No template/stub implementations** that exist just to make a test pass. Code must actually work.
- Keep functions small and focused on a single responsibility.

## Documentation

- Write **inline documentation** (godoc / Python docstrings) on every exported function, method, and type.
- Comments explain **WHY**, not WHAT. The code already shows what it does.
- Document non-obvious invariants, constraints, and security assumptions.
- Do NOT write multi-paragraph comments for trivial code.

## Testing

- Write **functional tests** that verify behaviour, not implementation details.
- Tests must test the **feature**, not the specific code path.
- **Do not write tests that are fitted to the code** — write them against the contract/spec first.
- Use real dependencies where possible (real DB in integration tests, not mocks).
- Tests must remain green. Never commit broken tests.

## Architecture

- Follow the **frozen contract** between Go backend and Python AI service (`/contracts/`). Do not change the contract without updating both sides.
- **Privacy flags** (`no_memory`, `no_training`, `no_summary`, `private`) must be respected at every layer.
- **Idempotency** must be maintained for all webhook processing, send operations, and approval callbacks.
- Risk classification and policy decisions belong to the backend, not the AI service.

## Refactoring & Backlog

- When you find **inefficient code** during work, add it to `/docs/backlog.md` under `## Inefficiency Backlog` instead of silently leaving it.
- When you find a **bug**, fix it immediately if small (< 10 lines). If large, add to `/docs/backlog.md` under `## Bug Backlog` and notify in your response.
- When you identify **technical debt**, add it to `/docs/backlog.md` under `## Tech Debt`.

## Security

- Never expose secrets, tokens, or credentials in logs or responses.
- All user-supplied text (Telegram messages) is **untrusted input** — wrap in XML tags before LLM calls.
- Validate at system boundaries (webhook input, external API responses). Trust internal code.
- Never bypass safety checks without explicit user instruction.

## Go-specific

- Use `slog` for structured logging. Never use `fmt.Println` in production paths.
- Errors must be handled or explicitly ignored with `_` and a comment explaining why.
- Avoid global mutable state. Pass dependencies via constructors.
- Use `sync.Map` or `sync.Mutex` for shared state. Never race on maps.

## Python-specific

- Use type annotations everywhere.
- Dataclasses with `frozen=True` for all request/response schemas.
- No bare `except:` — always catch specific exceptions.
- Keep AI service stateless — no in-memory caches between requests except via injected dependencies.

## Iteration 2 (deferred — do not implement without explicit instruction)

- Mini App UI (web dashboard)
- AI Roles Marketplace
- Telegram Client API / MTProto for historical message access
