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
  generation is rejected until drain retains the old owner, every in-flight
  operation and lease is zero for a continuous 60-second window, and
  `complete-drain` releases it. Every background write/renewal acquires the
  current generation/owner fence; an old generation never becomes valid again.
- Unknown `W/K/S` combinations are `DENY`. `ALLOW` requires an immutable smoke
  report and deployment identity. Release validation parses the real expanded
  Compose input, requires every image to use `@sha256`, and compares selected
  images plus checksums of the actual config/flags files with that identity.

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

Before selecting a target, add `--require-combination W1K1S1` (or the exact
target). The strict JSON report is bound to release id, combination, Web/Worker
digests, configuration, feature flags, and migration-manifest checksums. Every
named check must be true. Free-form `ok` files, all-DENY manifests, and reports
copied from another combination fail closed.

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
4. If `W1K0S1` is not ALLOW, close admission, drain K0, wait for the old
   process to stop accepting work and for in-flight/lease counts to remain zero
   for 60 seconds, complete drain, advance generation, and bind K1.
5. Validate a complete config using the candidate fragment while the live
   fragment is untouched. Only then atomically install, validate, and reload.
6. Run the target ALLOW smoke (ten readiness successes, running-container
   RepoDigest/release/config/flags/migration identity, and isolated critical
   write), then atomically activate admission and claims in one DB transition.
   A rejected activation leaves both gates closed.
7. Read old-Web WS counts directly from its loopback endpoint. Drain for up to
   five minutes and observe both versions for at least 60 minutes.

## Default rollback

Run `release.sh rollback-blue --execute` only in an approved window. Its order
is fixed: close admission → drain green worker → verify drained → advance the
generation fence → start blue worker claims-disabled → validate/reload the blue
Caddy fragment → run `SMOKE-W0K0S1/S2` → atomically activate both gates.
There is no open/enable intermediate state or weaker manual fallback.

## Observation and automatic action

Collect the preceding 15-minute baseline (at least 500 requests; otherwise use
documented absolute/synthetic checks), then run `observe.sh` on every window
continuously for at least 60 minutes. `OBSERVATION_COMMAND` is mandatory;
missing samples, command failure, or rollback signals fail immediately. A
short window exists only behind the explicit test-only switch. Exit 20 means
automatic rollback, 10 stops progress:

- rollback for auth/write/queue/WS smoke failure, duplicate claim/run, relay
  unhealthy, panic/OOM/restart, or data discrepancy;
- at >=100 requests/2m, rollback for 5xx >=5%, or >=5 errors and >=2x baseline;
  below 100 requests, rollback for three attributable 5xx/2m;
- at >=200 requests/5m, rollback after two p95 windows above both 2x baseline
  and baseline +500ms;
- CPU >=85% for three minutes stops; >=95% for one minute rolls back.
  MemAvailable <768MiB for 60 seconds stops; <512MiB rolls back. DB connections
  >=80/100 for 60 seconds rolls back.

`OBSERVATION_COMMAND` plugs this collector/decision loop into `release.sh`;
missing or invalid metrics never authorize progress.

## Installation, removal, and reverse rollback

Install immutable backend/web images plus this directory's Compose, Caddy
fragments, `release.sh`, `smoke.sh`, and `observe.sh`; the public Caddy config
imports one absolute managed-fragment path. Removal is the strict reverse:
close both gates, complete fenced drain, restore and independently
validate/reload the previous fragment, restore the legacy `all` service at its
immutable digest, run its ALLOW smoke, then remove transition-only services.
Never delete migration history or automatically run `migrate down`.

The old path is the single `MULTICA_PROCESS_ROLE=all` service. It remains a
compatibility path, not a bypass around target ALLOW, fencing, or rollback.
Unknown/misspelled roles exit before any migration attempt.

## Failure injection and isolated smoke

Use a dedicated local database/workspace and fake/stub external endpoints.
Block provider, agent CLI, Autopilot, mail, webhook, GitHub/Lark/Feishu/WeCom,
and third-party egress. Required negative cases are: missing Redis, invalid
role, remote unauthenticated admin bind, duplicate worker owner, manifest
checksum drift, unknown database migration, unknown/all-DENY combination,
forged/cross-combination report, pending contract migration, old-generation
activity, nonzero drain, Redis consumer failure/lag, Caddy validation/reload
failure, and rollback interruption. No cleanup substitutes for egress denial.

## Production rejection

This transition does not prove PITR/WAL, DB/attachment consistency, timed
RTO/RPO, host capacity, credentials, or any production digest combination.
Until those S3 gates and independent QA are complete, the only valid outcome is
"code/drill complete; production denied."
