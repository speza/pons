#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
state_dir="$(mktemp -d "${TMPDIR:-/tmp}/pons-runtime-smoke.XXXXXX")"
output_file="$state_dir/output.txt"
model="${PONS_SMOKE_MODEL:-gpt-5.6-luna}"

cleanup() {
	rm -rf -- "$state_dir"
}
trap cleanup EXIT

cd "$repo_root"
mkdir -p .build
go build -o .build/pons-hands ./cmd/pons-hands

# The default agent works in its own workspace; seed the file it reads.
agent_workspace="$state_dir/agents/default/workspace"
mkdir -p "$agent_workspace"
cp go.mod "$agent_workspace/"

prompt='Use the read_file tool to read go.mod. If its module line is exactly "module github.com/samperrin/pons", reply with exactly SMOKE_OK and nothing else. Otherwise reply with exactly SMOKE_FAILED and nothing else.'

# The run is local, so turn off any E2B-only GitHub App from the global
# config; explicit flags override it.
go run ./cmd/pons \
	-provider codex \
	-model "$model" \
	-sandbox seatbelt \
	-github-app-id 0 \
	-github-app-installation-id 0 \
	-github-app-private-key "" \
	-hands-command "$repo_root/.build/pons-hands" \
	-state-dir "$state_dir" \
	-debug \
	-message "$prompt" | tee "$output_file"

answer="$(awk 'NF { answer=$0 } END { print answer }' "$output_file")"
if [[ "$answer" != "SMOKE_OK" ]]; then
	printf 'runtime smoke test: expected SMOKE_OK, got %q\n' "$answer" >&2
	exit 1
fi

printf 'runtime smoke test: passed with %s\n' "$model"
