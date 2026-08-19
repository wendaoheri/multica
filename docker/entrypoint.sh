#!/bin/sh
set -e

# The legacy all-in-one role keeps its historical convenience default. Split
# Web/Worker roles never migrate on application startup: a release must run the
# checked one-shot migrator after validating its immutable manifest.
role="${MULTICA_PROCESS_ROLE:-all}"
case "$role" in
  all|web|worker) ;;
  *)
    echo "MULTICA_PROCESS_ROLE must be one of: all, web, worker" >&2
    exit 1
    ;;
esac
auto_migrate="${MULTICA_AUTO_MIGRATE:-}"
if [ -z "$auto_migrate" ]; then
  case "$role" in
    web|worker) auto_migrate=false ;;
    all) auto_migrate=true ;;
  esac
fi
case "$role:$auto_migrate" in
  web:true|web:1|worker:true|worker:1)
    echo "split process roles cannot run startup migrations; use the checked one-shot migrator" >&2
    exit 1
    ;;
esac
case "$auto_migrate" in
  true|1)
    echo "Running database migrations..."
    ./migrate up
    ;;
  false|0)
    echo "Automatic migrations disabled for process role: $role"
    ;;
  *)
    echo "MULTICA_AUTO_MIGRATE must be true or false" >&2
    exit 1
    ;;
esac

echo "Starting server..."
exec ./server
