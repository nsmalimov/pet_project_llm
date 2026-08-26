#!/bin/sh
# Runs the offline scenario scripts (examples/scenarios/*.json) against fresh
# copies of the reservations fixture with the mock executor — real worktrees,
# real `go test`, no LLM. Usage: scripts/run-scenarios.sh <data-dir> [work-dir]
set -eu
data="${1:?usage: run-scenarios.sh <data-dir> [work-dir]}"
work="${2:-$(mktemp -d)}"
root="$(cd "$(dirname "$0")/.." && pwd)"
for f in "$root"/examples/scenarios/*.json; do
  name="$(basename "$f" .json)"
  repo="$work/$name/reservations"
  rm -rf "$repo"; "$root/scripts/fixture-repo.sh" "$repo" >/dev/null
  echo "== $name"
  "$root/bin/orc" create --data "$data" --executor mock --script "$f" --repo "$repo" \
    --task "[$name] Fix duplicate reservation across timezones" \
    --repro-cmd "go test -run TestReserveRejectsSameUTCDayAcrossTimezones ./..." 2>&1 | grep -E "created task|packet.built|task.completed|decision.required" || true
done
