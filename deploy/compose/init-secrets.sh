#!/usr/bin/env bash
# Creates random tokens for the compose example. Existing tokens are kept.
set -euo pipefail
dir="$(dirname "$0")/../secrets"
mkdir -p "$dir"
umask 077
for name in sfu admin; do
  [ -s "$dir/$name.token" ] || head -c 32 /dev/urandom | base64 | tr -d '/+=\n' > "$dir/$name.token"
done
echo "tokens in $dir (the admin token goes in 'Authorization: Bearer <token>' for /api/)"
