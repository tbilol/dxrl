#!/usr/bin/env bash
# Creates one login role and one database per AGROGRAM service.
#
# The postgres image runs this from /docker-entrypoint-initdb.d only when the
# data volume is empty; on an existing volume it is skipped (see "Changing
# passwords" in deploy/README.md).
#
# For every database in `databases` below, the role with the same name:
#   - logs in with the password from <DATABASE>_DB_PASSWORD, e.g.
#     auth_gateway -> AUTH_GATEWAY_DB_PASSWORD (passed in by docker-compose.yml);
#   - owns the database, so it can create schemas and tables in it;
#   - is the only non-superuser that can connect to it, because CONNECT is
#     revoked from PUBLIC. Each service owns its data, and no two services
#     share a database.
#
# Passwords are passed as psql variables and quoted by format('%L'); they are
# never pasted into SQL text. Every statement is idempotent, so the script can
# be re-run by hand, e.g. after a password change:
#   docker compose exec postgres /docker-entrypoint-initdb.d/01-create-service-databases.sh
#
# The entrypoint executes this file when it is executable and sources it
# otherwise. Both work: the logic lives in a function, and the shell options
# below are the ones the entrypoint already uses.
set -Eeo pipefail

create_service_databases() {
  # One entry per service that keeps data in PostgreSQL. A new entry also
  # needs its <DATABASE>_DB_PASSWORD in docker-compose.yml and .env.example.
  local -a databases=(auth_gateway identity profile_social catalog order_escrow)
  local db password_var password

  # Check everything first, so a bad setting changes nothing.
  for db in "${databases[@]}"; do
    password_var="${db^^}_DB_PASSWORD"
    if [ -z "${!password_var:-}" ]; then
      echo "create-service-databases: $password_var is empty; set it in deploy/.env" >&2
      return 1
    fi
    if [ "$db" = "$POSTGRES_USER" ]; then
      echo "create-service-databases: the superuser must not be named like a service database ($db)" >&2
      return 1
    fi
  done

  for db in "${databases[@]}"; do
    password_var="${db^^}_DB_PASSWORD"
    password="${!password_var}"
    echo "create-service-databases: role and database \"$db\""
    psql --no-psqlrc --no-password --quiet --set ON_ERROR_STOP=1 \
      --username "$POSTGRES_USER" --dbname postgres \
      --set db="$db" --set password="$password" <<'EOSQL'
SELECT format('CREATE ROLE %I', :'db')
WHERE NOT EXISTS (SELECT FROM pg_catalog.pg_roles WHERE rolname = :'db')
\gexec
SELECT format('ALTER ROLE %I WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE PASSWORD %L', :'db', :'password')
\gexec
SELECT format('CREATE DATABASE %I OWNER %I', :'db', :'db')
WHERE NOT EXISTS (SELECT FROM pg_catalog.pg_database WHERE datname = :'db')
\gexec
SELECT format('ALTER DATABASE %I OWNER TO %I', :'db', :'db')
\gexec
SELECT format('REVOKE ALL ON DATABASE %I FROM PUBLIC', :'db')
\gexec
EOSQL
  done
}

create_service_databases
