#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
# postgres initdb.d script: create the third-party service databases
# (identity plane: hydra + kratos; webhooks: svix).
#
# Ory Hydra and Ory Kratos BOTH create a `networks` table (and other
# same-named relations) in whatever database their DSN points at. If
# they share one database, whichever migrates second dies with
# `ERROR: relation "networks" already exists` (this exact failure
# broke the CI smoke job). Each third-party service therefore gets its
# OWN database; the Sutra application keeps using `sutra`. The Svix
# server (G2) gets `svix` for the same reason — its `message` table
# sits one letter away from the chat service's `messages` table, and
# shared-database drift between independent products is how the
# Hydra/Kratos outage happened.
#
# Runs once, when the pgdata volume is first initialized. Idempotent
# (\gexec only emits CREATE DATABASE when the database is missing),
# and harmless on later restarts. If a pgdata volume was created
# before this file existed, create the databases by hand once:
#   docker compose exec postgres psql -U sutra -d sutra \
#     -c "SELECT 'CREATE DATABASE hydra' WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'hydra')\gexec" \
#     -c "SELECT 'CREATE DATABASE kratos' WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'kratos')\gexec" \
#     -c "SELECT 'CREATE DATABASE svix' WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'svix')\gexec"
set -e

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<'EOSQL'
SELECT 'CREATE DATABASE hydra'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'hydra')\gexec
SELECT 'CREATE DATABASE kratos'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'kratos')\gexec
SELECT 'CREATE DATABASE svix'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'svix')\gexec
EOSQL
