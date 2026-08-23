#!/bin/sh
# Critic files spec-less Powder drafts; without a configured Powder origin and
# agent there is no durable output. Report a healthy skip instead of waking a
# daily sweep that cannot file anything.
if [ -n "${POWDER_AGENT:-}" ] && { [ -n "${POWDER_URL:-}" ] || [ -n "${POWDER_API_BASE_URL:-}" ]; }; then
	exit 0
fi
exit 1
