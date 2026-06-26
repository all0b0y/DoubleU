SHELL := /usr/bin/env bash

.PHONY: contracts contracts-check test test-go test-python migrate-up migrate-down migrate-status models-apply

# Regenerate Go + Python enum files from contracts/contracts.json.
contracts:
	python3 scripts/generate_contracts.py

# CI check: regenerate and fail if anything is out of date.  Run in clean tree.
contracts-check:
	python3 scripts/generate_contracts.py
	git diff --exit-code -- backend-go/internal/contracts/contract_enums.gen.go ai-python/ai_agent/contract_enums.py

test: test-go test-python

test-go:
	cd backend-go && go test ./... -count=1

test-python:
	cd ai-python && python3 -m unittest discover -s tests

# Re-render LiteLLM's model_list from .env and recreate both model consumers.
models-apply:
	docker compose up -d --force-recreate litellm ai-service

# Migration helpers (require DATABASE_URL).  See cmd/backend/main.go runMigrateCommand.
migrate-up:
	cd backend-go && go run ./cmd/backend migrate up

migrate-down:
	cd backend-go && go run ./cmd/backend migrate down $(VERSION)

migrate-status:
	cd backend-go && go run ./cmd/backend migrate status
