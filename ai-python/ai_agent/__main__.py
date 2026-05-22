"""Command-line entrypoint for the stdlib AI HTTP service."""

from __future__ import annotations

import os

from .http_server import run


def main() -> None:
    """Start the AI HTTP service using environment configuration."""
    host = os.getenv("AI_HOST", "0.0.0.0")
    port = int(os.getenv("AI_PORT", "8081"))
    run(host=host, port=port)


if __name__ == "__main__":
    main()
