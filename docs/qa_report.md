# QA and Security Report

## Current Status

QA gate for the Technical MVP scaffold is passing for local non-Docker checks.

- Go tests pass.
- Python tests pass.
- Contract/security checks pass.
- Go coverage meets the first-stage threshold: 70.0%.
- Python coverage meets the first-stage threshold: 70.9%.
- Local healthchecks pass for backend and AI service.
- Local end-to-end smoke passes for manual draft, auto reply, prompt-injection blocking, duplicate webhook handling, duplicate approval callback handling, and audit visibility.

Docker Compose startup was not fully executed in this environment because the local Docker daemon was unavailable:

```text
Cannot connect to the Docker daemon at unix:///Users/all0b0y/.docker/run/docker.sock.
```

The repository still includes Docker Compose, Dockerfiles, healthchecks, migrations, and seed data so the same check can be run when Docker is available.

## Strict Gates

- Contract drift: `python3 scripts/check_policy_matrix.py`.
- Go coverage: `scripts/check_go_coverage.sh`.
- Python coverage: `scripts/check_python_coverage.sh`.
- Combined local gate: `scripts/coverage-check.sh`.
- Contract/security gate: `scripts/contract-security-tests.sh`.
- Migration/seed static smoke: `scripts/migration-seed-smoke.sh`.

## Verified Behaviors

- User/sender messages are treated as untrusted data in Python prompt construction.
- Direct generation goes through `ModelRouter -> LLMGateway`.
- Every inbound backend path creates an audit event after policy/privacy evaluation.
- Duplicate Telegram updates return `decision=ignore` and do not create duplicate replies.
- Duplicate approval callbacks return `duplicate=true` and do not create duplicate replies.
- `private` prevents backend auto reply.
- `no_memory` blocks memory extraction in the AI service.
- `no_summary` blocks summary generation in the AI service.
- Prompt injection is classified high risk and is blocked from autonomous reply in auto mode.

## Required Test Commands

```bash
cd backend-go && go test ./... -coverprofile=../coverage-go.out
python3 scripts/run_python_unittest.py
python3 scripts/check_policy_matrix.py
scripts/contract-security-tests.sh
scripts/coverage-check.sh
```

When Docker is available:

```bash
docker compose up --build
scripts/migration-seed-smoke.sh
```

## Remaining Risks

- Runtime persistence is currently represented by in-memory repositories in the Go backend; Postgres schema and seed data are present for the next integration step.
- The Telegram sender is a no-op in local MVP mode unless a real Bot API sender and token are configured.
- Docker startup could not be verified in this host session because Docker was not running.
