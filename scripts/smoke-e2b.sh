#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [[ -z "${E2B_API_KEY:-}" ]]; then
	printf 'E2B smoke test: E2B_API_KEY is required\n' >&2
	exit 1
fi

cd "$repo_root"

PONS_E2B_TEST=1 go test \
	-tags integration \
	-count=1 \
	-run '^TestE2BHandsRoundTrip$' \
	-v ./environment/e2b

printf 'E2B smoke test: passed\n'
