#!/bin/sh
# Instantiates the reservations fixture as a standalone git repository with
# the bug committed, so orc can create a worktree from it.
#   scripts/fixture-repo.sh /path/to/dest
set -eu
dest="${1:?usage: fixture-repo.sh <dest-dir>}"
src="$(cd "$(dirname "$0")/../fixtures/reservations" && pwd)"
mkdir -p "$dest"
cp "$src"/* "$dest"/
cd "$dest"
git init -q
git -c user.name=fixture -c user.email=fixture@localhost add -A
git -c user.name=fixture -c user.email=fixture@localhost commit -q -m "reservations service with timezone duplicate bug"
echo "$dest"
