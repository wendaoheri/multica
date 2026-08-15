#!/usr/bin/env bash
#
# log-redact.sh — line-oriented secret scrubber for shipped logs (PER-284).
#
# Purpose:
#   Log forwarding must never carry secrets off the server. The backend
#   already scrubs its own slog output at the source
#   (server/internal/logger/redact_handler.go), but docker logs also carry
#   frontend/postgres/migration stderr and anything that bypasses the Go
#   logger. This filter is the second, shipping-time layer: pipe raw logs
#   through it BEFORE persisting or forwarding anywhere.
#
# Usage:
#   some_log_source | log-redact.sh > scrubbed.log
#
# Pattern families mirror server/pkg/redact/redact.go. The direction is
# deliberately fail-safe: over-redaction is acceptable, leaking is not.
# Unknown secret shapes can still slip through (this is regex, not a
# parser) — which is exactly why source-level redaction stays the primary
# guarantee and this is defense-in-depth.
#
# Portability: BSD awk (macOS) and gawk. No {n,m} intervals, no \b, no
# (?i) — macOS ships a POSIX awk that predates those.

set -euo pipefail

awk '
# --- PEM private keys: collapse a multi-line block to one placeholder ---
/-----BEGIN[ A-Z]*PRIVATE KEY-----/ { in_pem = 1 }
in_pem {
    if ($0 ~ /-----END[ A-Z]*PRIVATE KEY-----/) {
        in_pem = 0
        print "[REDACTED PRIVATE KEY]"
    }
    next
}

{
    line = $0

    # AWS access key IDs (AKIA + uppercase alnum run)
    gsub(/AKIA[0-9A-Z]+/, "[REDACTED AWS KEY]", line)

    # GitHub tokens: fine-grained first, then classic/oauth/app prefixes
    gsub(/github_pat_[A-Za-z0-9_]+/, "[REDACTED GITHUB TOKEN]", line)
    gsub(/gh[pousr]_[A-Za-z0-9_]+/, "[REDACTED GITHUB TOKEN]", line)

    # Stripe live keys (underscore form; the sk- rule below does not match)
    gsub(/(sk|rk)_live_[0-9A-Za-z]+/, "[REDACTED STRIPE KEY]", line)

    # OpenAI / Anthropic style sk- keys
    gsub(/sk-[A-Za-z0-9_-]+/, "[REDACTED API KEY]", line)

    # Slack bot/user/app-level tokens
    gsub(/xox[bporase]-[A-Za-z0-9-]+/, "[REDACTED SLACK TOKEN]", line)
    gsub(/xapp-[A-Za-z0-9-]+/, "[REDACTED SLACK TOKEN]", line)

    # GitLab personal access tokens
    gsub(/glpat-[A-Za-z0-9_-]+/, "[REDACTED GITLAB TOKEN]", line)

    # Google API keys (AIza prefix)
    gsub(/AIza[0-9A-Za-z_-]+/, "[REDACTED GOOGLE API KEY]", line)

    # JWTs (three base64url segments). Over-redacting long dotted base64
    # is acceptable here; missing a real JWT is not.
    gsub(/ey[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+/, "[REDACTED JWT]", line)

    # Bearer tokens
    gsub(/[Bb]earer[ \t]+[A-Za-z0-9._~+\/=-]+/, "Bearer [REDACTED]", line)

    # Connection strings with embedded credentials
    gsub(/(postgres|postgresql|mysql|mongodb|redis|amqp):\/\/[^:@\/ ]+:[^@ ]+@/, "[REDACTED CONNECTION STRING]@", line)

    # Generic credential assignments (KEY=value / KEY: value / "KEY":"value").
    # Keywords are spelled with [Cc]haracter classes for case-insensitivity
    # (portable awk has no (?i)).
    gsub(/([Aa][Pp][Ii][_][Kk][Ee][Yy]|[Aa][Pp][Ii][_][Ss][Ee][Cc][Rr][Ee][Tt]|[Ss][Ee][Cc][Rr][Ee][Tt][_][Kk][Ee][Yy]|[Ss][Ee][Cc][Rr][Ee][Tt]|[Aa][Cc][Cc][Ee][Ss][Ss][_][Tt][Oo][Kk][Ee][Nn]|[Aa][Uu][Tt][Hh][_][Tt][Oo][Kk][Ee][Nn]|[Pp][Rr][Ii][Vv][Aa][Tt][Ee][_][Kk][Ee][Yy]|[Dd][Aa][Tt][Aa][Bb][Aa][Ss][Ee][_][Uu][Rr][Ll]|[Dd][Bb][_][Pp][Aa][Ss][Ss][Ww][Oo][Rr][Dd]|[Dd][Bb][_][Uu][Rr][Ll]|[Rr][Ee][Dd][Ii][Ss][_][Uu][Rr][Ll]|[Pp][Aa][Ss][Ss][Ww][Oo][Rr][Dd]|[Tt][Oo][Kk][Ee][Nn])[ \t]*["'"'"']?[=:][ \t]*["'"'"']?[^ \t,}"'"'"']+/, "[REDACTED CREDENTIAL]", line)

    # Home-directory paths: hide the account name
    gsub(/\/Users\/[A-Za-z0-9._-]+/, "/Users/****", line)
    gsub(/\/home\/[A-Za-z0-9._-]+/, "/home/****", line)

    print line
}
' "$@"
