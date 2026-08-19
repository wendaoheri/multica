#!/bin/sh
set -eu
root=$(CDPATH= cd -- "$(dirname "$0")" && pwd); work=$(mktemp -d); trap 'rm -rf "$work"' EXIT HUP INT TERM
mkdir "$work/bin" "$work/artifacts"; : >"$work/body.json"
d=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
cat >"$work/artifacts/manifest.json" <<EOF
{"release_id":"r1","migration_manifest_checksum":"$d","combinations":{"W1K1S1":{"deployment":{"web_image":"r/web@sha256:$d","worker_image":"r/worker@sha256:$d","frontend_image":"r/front@sha256:$d","migrator_image":"r/migrate@sha256:$d","config_checksum":"$d","feature_flags_checksum":"$d","migration_manifest_checksum":"$d"}}}}
EOF
cat >"$work/runtime.good.json" <<EOF
{"release_id":"r1","combination":"W1K1S1","migration_manifest_checksum":"$d","deployment":{"web_image":"r/web@sha256:$d","worker_image":"r/worker@sha256:$d","frontend_image":"r/front@sha256:$d","migrator_image":"r/migrate@sha256:$d","config_checksum":"$d","feature_flags_checksum":"$d","migration_manifest_checksum":"$d"}}
EOF
cat >"$work/bin/releasectl" <<'EOF'
#!/bin/sh
exit 0
EOF
cat >"$work/bin/curl" <<'EOF'
#!/bin/sh
case "$*" in *'/readyz'*) printf '{"ready":true}\n' ;; *'/status'*) printf '{"control":{"active_generation":2,"claims_enabled":false,"admission_open":false}}\n' ;; *'write-out'*) printf '200' ;; *) printf '200' ;; esac
EOF
chmod +x "$work/bin/"*
run() {
  PATH="$work/bin:$PATH" COMBINATION=W1K1S1 WEB_URL=http://127.0.0.1:1 FRONTEND_URL=http://127.0.0.1:2 \
  WORKER_ADMIN_URL=http://127.0.0.1:3 CRITICAL_WRITE_URL=http://127.0.0.1:4 MULTICA_WORKER_ADMIN_TOKEN=x \
  RELEASE_ARTIFACT_DIR="$work/artifacts" RELEASECTL=releasectl CRITICAL_WRITE_BODY_FILE="$work/body.json" \
  REPORT="$work/report.json" RUNTIME_IDENTITY_REPORT="$work/runtime.json" RUNTIME_IDENTITY_COMMAND="$1" "$root/smoke.sh"
}
run "cp '$work/runtime.good.json' '$work/runtime.json'"
jq -e '.runtime_identity.release_id == "r1"' "$work/report.json" >/dev/null
sed 's/"r1"/"wrong"/' "$work/runtime.good.json" >"$work/runtime.bad.json"
if run "cp '$work/runtime.bad.json' '$work/runtime.json'" >/dev/null 2>&1; then echo "mismatched runtime identity accepted" >&2; exit 1; fi
if run true >/dev/null 2>&1; then echo "missing runtime identity accepted" >&2; exit 1; fi
echo "runtime release/image/config/flags/migration identity fail-closed smoke: PASS"
