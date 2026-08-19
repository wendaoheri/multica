#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT HUP INT TERM
cp "$root/docker/entrypoint.sh" "$work/entrypoint.sh"
chmod +x "$work/entrypoint.sh"

cat >"$work/migrate" <<'EOF'
#!/bin/sh
printf 'migrate:%s\n' "$*" >>calls
EOF
cat >"$work/server" <<'EOF'
#!/bin/sh
printf 'server\n' >>calls
EOF
chmod +x "$work/migrate" "$work/server"

(
  cd "$work"
  : >calls
  MULTICA_PROCESS_ROLE=all ./entrypoint.sh
  grep -qx 'migrate:up' calls
  grep -qx 'server' calls

  : >calls
  MULTICA_PROCESS_ROLE=web ./entrypoint.sh
  test "$(cat calls)" = server

  : >calls
  if MULTICA_PROCESS_ROLE=worker MULTICA_AUTO_MIGRATE=true ./entrypoint.sh; then
    echo "worker startup migration unexpectedly succeeded" >&2
    exit 1
  fi
  test ! -s calls
)

echo "entrypoint role/migration isolation: PASS"
