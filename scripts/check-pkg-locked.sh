#!/usr/bin/env bash
# Fails if this branch changed the public pkg/ contract or the served OpenAPI
# spec. The digest feature is deliberately confined to internal/ and cmd/.
set -euo pipefail

BASE="${1:-origin/main}"

if ! git rev-parse --verify "$BASE" >/dev/null 2>&1; then
  echo "check-pkg-locked: base ref '$BASE' not found" >&2
  exit 2
fi

changed="$(git diff --name-only "$BASE"...HEAD -- pkg/ || true)"
if [ -n "$changed" ]; then
  echo "check-pkg-locked: pkg/ must not change on this branch:" >&2
  echo "$changed" >&2
  exit 1
fi

echo "check-pkg-locked: pkg/ is unchanged against $BASE"
