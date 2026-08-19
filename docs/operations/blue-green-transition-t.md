# Blue/green transition T

This runbook is the repository-owned implementation contract for PER-376. It
does not authorize production use. The project-specific host facts remain in
the operator's `ops/runbook.md`; production preparation starts only after an
independent reviewer approves this implementation and a separate S3 Go/No-Go.

## Safety invariants

- `MULTICA_PROCESS_ROLE=all` preserves the existing single-process behavior.
- `web` and `worker` are split roles. They require Redis, reject legacy relay
  mode, disable startup migration, and fail closed when the Redis probe fails.
- Web owns HTTP/WS, daemon sockets, and `BatchedHeartbeatScheduler`. Worker owns
  runtime/autopilot sweepers, scheduler, webhook delivery, PR refresh,
  channel/media, and DB stats. Worker never starts the business listener.
- Worker management is loopback-only, or bearer-authenticated with a token of
  at least 32 bytes when a container bridge listener is required. Host ports in
  the transition Compose file bind only to `127.0.0.1`.
- PostgreSQL `deployment_release_control` is the generation fence. Advancing a
  generation atomically closes admission and claims and removes the old owner.
  A worker starts claims-disabled and only one owner can enable claims.
- Unknown `W/K/S` combinations are `DENY`. `ALLOW` requires an immutable smoke
  report whose checksum matches the release manifest.

## Build artifacts

Build backend and web images and record their repository digests, not tags.
The backend image contains `server`, `migrate`, and `releasectl`. Generate a
release directory containing `manifest.json`, smoke reports, the exact feature
flag file, configuration checksum (secret values hashed without logging), and
the rollback script checksum. Validate it with:

```sh
releasectl verify-manifest \
  --manifest "$RELEASE_ARTIFACT_DIR/manifest.json" \
  --artifact-dir "$RELEASE_ARTIFACT_DIR" \
  --migration-dir /app/migrations \
  --check-database
```

The database comparison rejects missing, extra, reordered, modified, or
misclassified migrations. Pending files may only be `none` or `expand`.
Contract/M3 never enters a normal blue/green artifact.

## One-shot migration

Application containers set `MULTICA_AUTO_MIGRATE=false`. Run only the Compose
`migrator` profile after manifest validation. It applies PostgreSQL
`lock_timeout=3s`, `statement_timeout=15min`, and an outer 20-minute deadline.
Failure leaves Web/Worker stopped or fenced; inspect the manifest, invalid
concurrent indexes, pre-hooks, and backfill watermark, then forward-fix. Do not
automatically run `migrate down`.

## Cutover

1. Acquire the host release lock. Confirm immutable digests/checksums, an
   independently restored backup, capacity thresholds, and the exact ALLOW
   combinations. Any unknown is DENY.
2. Run the one-shot expand migrator.
3. Start candidate Web and Worker; Worker remains claims-disabled. Candidate
   `/readyz` must pass ten times and the relay/worker status probe must pass.
4. If `W1K0S1` is not ALLOW, close admission, drain K0, wait for
   `accepting_new=false`, advance generation, and bind K1. Never overlap owners.
5. Validate the complete Caddy configuration, atomically install the managed
   green fragment, and reload. Do not use random percentage canary.
6. Run the immutable combination smoke, then enable claims/open admission.
7. Read old-Web WS counts directly from its loopback endpoint. Drain for up to
   five minutes and observe both versions for at least 60 minutes.

## Default rollback

Run `release.sh rollback-blue --execute` only in an approved window. Its order
is fixed: close admission → drain green worker → verify drained → advance the
generation fence → start blue worker claims-disabled → validate/reload the blue
Caddy fragment → run `SMOKE-W0K0S1/S2` → enable blue claims → reopen admission.
Any uncertain query keeps admission and claims closed for manual handling.

## Failure injection and isolated smoke

Use a dedicated local database/workspace and fake/stub external endpoints.
Block provider, agent CLI, Autopilot, mail, webhook, GitHub/Lark/Feishu/WeCom,
and third-party egress. Required negative cases are: missing Redis, invalid
role, remote unauthenticated admin bind, duplicate worker owner, manifest
checksum drift, unknown combination, pending contract migration, generation
change while running, and Caddy validation failure. No cleanup step substitutes
for egress denial.

## Production rejection

This transition does not prove PITR/WAL, DB/attachment consistency, timed
RTO/RPO, host capacity, credentials, or any production digest combination.
Until those S3 gates and independent QA are complete, the only valid outcome is
"code/drill complete; production denied."
