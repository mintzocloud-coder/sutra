-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Migration 0002: users table.
--
-- Users are identified by the OIDC subject (stable, issuer-scoped), which
-- is the same identity the chat service uses for socket auth — one
-- identity across modules. Rows are created lazily on first authenticated
-- call ("just-in-time provisioning", the standard OIDC pattern):
-- see internal/users.Provision.
--
-- handle is the public, user-chosen identifier (unique). It starts NULL on
-- JIT-provisioned rows and is claimed later via the profile endpoint
-- (future task); UNIQUE permits multiple NULLs in Postgres, which is
-- exactly what we want here.
--
-- Safe to re-run: IF NOT EXISTS + CREATE OR REPLACE + DROP IF EXISTS.

CREATE TABLE IF NOT EXISTS users (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    subject      text        NOT NULL UNIQUE,
    handle       text        UNIQUE,
    display_name text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- updated_at is maintained by the database so writers can never forget
-- to bump it. Shared function (name is generic on purpose): later tables
-- attach the same trigger.
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS users_set_updated_at ON users;
CREATE TRIGGER users_set_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
