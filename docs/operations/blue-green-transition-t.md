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
  Compose input with the `migration` profile, requires exactly seven services
  and every image to use `@sha256`, and compares selected images plus checksums
  of the actual config/flags inputs with that identity.
- `RELEASE_CONFIG_FILE` is the one canonical backend `KEY=VALUE` env file and
  `RELEASE_FEATURE_FLAGS_FILE` is the one mounted flags file. Compose consumes
  those exact paths; validation compares the expanded environment and mounts
  to the normalized source without persisting or logging expanded secret
  values. A side copy, path alias, tag, or changed content fails closed.

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

The migrator uses the same `--profile migration` expansion and verified
config/flags mounts as release validation. Its fixed command order is:

1. `begin-migrator-attempt` atomically replaces
   `RELEASE_EXECUTION_DIR/current-attempt.json` with a cryptographically random,
   non-reusable `attempt_id`, release/combination, complete deployment identity,
   and RFC3339 `started_at`. The same command writes only the nonce to a `0600`
   one-shot-private file created inside the migrator container; it does not
   print the nonce;
2. verify manifest/migrations and run `migrate up`;
3. `record-migrator-execution --expected-attempt-id "$(cat "$attempt_file")"`
   explicitly carries that invocation's private nonce and atomically writes
   the matching attempt id, `started_at`, RFC3339 `completed_at`, and deployment
   identity.

Beginning the attempt is the first command. If it cannot persist, no verify or
migration command runs. Once a retry begins, its new current attempt immediately
invalidates every older success record, even for the same release, combination,
and deployment. A verify, migration, or completion-write failure therefore
leaves the new attempt unmatched and smoke stays closed. Runtime smoke requires
both files, exact nonce/binding equality, strict timestamps, and
`completed_at >= started_at`; it uses no recency window or file mtime.

Completion never derives its nonce from the shared current-attempt file. It
validates the private expected nonce, rereads shared current, and writes a
success record only when they match. If another one-shot replaces current
before that check, completion fails. If replacement races after the check but
before atomic success installation, the success remains bound to the earlier
private nonce and runtime verification rejects current/success mismatch. The
host release lock is defense in depth, not a correctness prerequisite.

The durable files remain available after the one-shot container exits or is
removed, so smoke never depends on default `compose ps -q` finding a cleaned
migrator. Neither file contains config values or credentials.

```sh
docker compose --profile migration \
  -f deploy/bluegreen/docker-compose.transition.yml run --rm migrator
test -s "$MIGRATOR_CURRENT_ATTEMPT"
test -s "$MIGRATOR_EXECUTION_RECORD"
```

`run --rm` is safe here because begin and completion records use the
host-mounted execution directory before Compose removes the container. Do not
reorder the command chain, reuse an attempt id, or substitute an unprofiled
`compose run`, a container label, a timestamp-age check, or file mtime.
The private attempt file is created by `mktemp`, removed by an EXIT trap, and is
never placed in the host-mounted execution directory or emitted to logs.

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
   RepoDigest/release/config/flags identity, durable completed-migrator identity,
   and isolated critical write), then atomically activate admission and claims
   in one DB transition. A rejected activation leaves both gates closed.
7. Read old-Web WS counts directly from its loopback endpoint. Drain for up to
   five minutes and observe both versions for at least 60 minutes.

## Default rollback

Run `release.sh rollback-blue --execute` only in an approved window. Its order
is fixed: close admission → drain green worker → verify drained → advance the
generation fence → start blue worker claims-disabled → validate/reload the blue
Caddy fragment → run `SMOKE-W0K0S1/S2` → atomically activate both gates.
There is no open/enable intermediate state or weaker manual fallback.
The management API exposes only `/activate` for opening both gates. The former
`/enable-claims` and `/admission/open` routes return 404, and the corresponding
`enable-claims` / `admission-open` CLI commands are unknown. `/admission/close`,
drain, and complete-drain remain one-way fail-close operations.

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
