#!/usr/bin/env bash
#
# Live smoke test of one provider across several models: each model must
# call a tool and answer. Needs the provider's key (or codex login); runs
# in the sandbox from your pons config.
#
#   scripts/smoke-providers.sh                         # opencode-go, one model per API
#   scripts/smoke-providers.sh openrouter openrouter/auto
#   scripts/smoke-providers.sh opencode-go qwen3.8-max kimi-k3

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
provider="${1:-opencode-go}"
shift || true
models=("$@")
if [[ ${#models[@]} -eq 0 ]]; then
	case "$provider" in
	opencode-go) models=(glm-5.3-flash qwen3.8-flash grok-4.7) ;; # completions, messages, responses
	*)
		echo "usage: $0 <provider> <model>..." >&2
		exit 2
		;;
	esac
fi

# OpenCode's own login works when the key is not exported.
opencode_auth="$HOME/.local/share/opencode/auth.json"
if [[ "$provider" == opencode-go && -z "${OPENCODE_API_KEY:-}" && -f "$opencode_auth" ]]; then
	OPENCODE_API_KEY="$(jq -r '(."opencode-go" // .opencode).key // empty' "$opencode_auth")"
	export OPENCODE_API_KEY
fi

work_dir="$(mktemp -d "${TMPDIR:-/tmp}/pons-provider-smoke.XXXXXX")"
cleanup() {
	rm -rf -- "$work_dir"
}
trap cleanup EXIT

cd "$repo_root"
mkdir -p .build
go build -o .build/pons ./cmd/pons

# A tiny workspace: sandboxes that copy the workspace in (E2B) cap its size.
workspace="$work_dir/workspace"
mkdir -p "$workspace"
cp go.mod "$workspace/"

prompt='Use the read_file tool to read go.mod. If its module line is exactly "module github.com/samperrin/pons", reply with exactly SMOKE_OK and nothing else. Otherwise reply with exactly SMOKE_FAILED and nothing else.'

failed=()
for model in "${models[@]}"; do
	printf '\n== %s %s\n' "$provider" "$model"
	state_dir="$work_dir/$(tr '/' '_' <<<"$model")"
	output_file="$state_dir.out"
	# This checks providers, not workspaces: let the client pick the tiny
	# workspace whatever the sandbox's default policy.
	mkdir -p "$state_dir/agents/default"
	printf '{"workspace": "per_conversation"}\n' >"$state_dir/agents/default/agent.json"
	# Keep going on failure so one run reports every model.
	if ! .build/pons \
		-provider "$provider" \
		-model "$model" \
		-state-dir "$state_dir" \
		-workspace "$workspace" \
		-workspace-root "$work_dir" \
		-message "$prompt" >"$output_file" 2>&1; then
		:
	fi
	answer="$(awk 'NF { answer=$0 } END { print answer }' "$output_file")"
	if [[ "$answer" == "SMOKE_OK" ]]; then
		echo "passed"
	else
		failed+=("$model")
		# The error, if any, is in the log; show its tail.
		tail -n 5 "$output_file"
	fi
done

printf '\n'
if [[ ${#failed[@]} -gt 0 ]]; then
	printf '%s: %d of %d failed: %s\n' "$provider" "${#failed[@]}" "${#models[@]}" "${failed[*]}" >&2
	exit 1
fi
printf '%s: all %d passed\n' "$provider" "${#models[@]}"
