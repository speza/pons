#!/usr/bin/env bash

set -euo pipefail

template="${E2B_TEMPLATE:-pons-hands}"
all=false
dry_run=false
cli_version="${E2B_CLI_VERSION:-2.20.0}"

usage() {
	cat <<'EOF'
Usage: scripts/cleanup-e2b.sh [--template NAME | --all] [--dry-run]

Kills running and paused E2B sandboxes. By default, only sandboxes created
from the pons-hands template are selected. Use --all explicitly to select every
sandbox visible to E2B_API_KEY.

Options:
  --template NAME  select one template or alias (default: pons-hands)
  --all            select all templates in the E2B project
  --dry-run        print matching sandbox IDs without killing them
  -h, --help       show this help
EOF
}

while (($#)); do
	case "$1" in
		--template)
			if (($# < 2)) || [[ -z "$2" ]]; then
				printf 'cleanup-e2b: --template requires a value\n' >&2
				exit 2
			fi
			template="$2"
			all=false
			shift 2
			;;
		--all)
			all=true
			shift
			;;
		--dry-run)
			dry_run=true
			shift
			;;
		-h|--help)
			usage
			exit 0
			;;
		*)
			printf 'cleanup-e2b: unknown argument %q\n' "$1" >&2
			usage >&2
			exit 2
			;;
	esac
done

if [[ -z "${E2B_API_KEY:-}" ]]; then
	printf 'cleanup-e2b: E2B_API_KEY is required\n' >&2
	exit 1
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

cli=(npx --yes "@e2b/cli@${cli_version}")
tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/pons-e2b-cleanup.XXXXXX")"
trap 'rm -rf -- "$tmp_dir"' EXIT

for state in running paused; do
	args=(sandbox list --state "$state" --format json --limit 0)
	if [[ "$all" == false ]]; then
		args+=(--template "$template")
	fi
	"${cli[@]}" "${args[@]}" >"$tmp_dir/$state.json"
done

python3 - "$tmp_dir/running.json" "$tmp_dir/paused.json" >"$tmp_dir/ids" <<'PY'
import json
import sys

seen = set()
for path in sys.argv[1:]:
    with open(path, encoding="utf-8") as source:
        for sandbox in json.load(source):
            sandbox_id = sandbox.get("sandboxID") or sandbox.get("sandboxId")
            if sandbox_id and sandbox_id not in seen:
                seen.add(sandbox_id)
                print(sandbox_id)
PY

sandbox_ids=()
while IFS= read -r sandbox_id; do
	[[ -n "$sandbox_id" ]] && sandbox_ids+=("$sandbox_id")
done <"$tmp_dir/ids"

if ((${#sandbox_ids[@]} == 0)); then
	if [[ "$all" == true ]]; then
		printf 'cleanup-e2b: no running or paused sandboxes found\n'
	else
		printf 'cleanup-e2b: no running or paused sandboxes found for template %q\n' "$template"
	fi
	exit 0
fi

if [[ "$all" == true ]]; then
	printf 'cleanup-e2b: matched %d sandbox(es) across all templates:\n' "${#sandbox_ids[@]}"
else
	printf 'cleanup-e2b: matched %d sandbox(es) for template %q:\n' "${#sandbox_ids[@]}" "$template"
fi
printf '  %s\n' "${sandbox_ids[@]}"

if [[ "$dry_run" == true ]]; then
	printf 'cleanup-e2b: dry run; no sandboxes killed\n'
	exit 0
fi

# Keep each command comfortably below platform argument limits.
for ((offset = 0; offset < ${#sandbox_ids[@]}; offset += 100)); do
	"${cli[@]}" sandbox kill "${sandbox_ids[@]:offset:100}"
done

printf 'cleanup-e2b: killed %d sandbox(es)\n' "${#sandbox_ids[@]}"
