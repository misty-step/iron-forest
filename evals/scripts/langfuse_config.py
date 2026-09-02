"""Shared Langfuse endpoint validation for evaluation maintenance clients."""

from __future__ import annotations

from urllib.parse import urlsplit

DEFAULT_BASE_URL = "https://cloud.langfuse.com"


def normalized_base_url(raw: str | None) -> str:
    value = (raw or "").strip()
    if len(value) >= 2 and value[0] == value[-1] and value[0] in {"'", '"'}:
        value = value[1:-1].strip()
    if not value:
        return DEFAULT_BASE_URL
    if "://" not in value:
        value = f"https://{value}"
    parsed = urlsplit(value)
    if parsed.scheme not in {"http", "https"} or not parsed.netloc:
        raise ValueError("LANGFUSE_BASE_URL must be an HTTP(S) URL or host name")
    return value.rstrip("/")
