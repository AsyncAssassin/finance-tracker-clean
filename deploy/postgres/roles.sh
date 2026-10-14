#!/bin/sh
# Used by initdb and explicitly during upgrades. Run only as the DB administrator.
set -eu
: "${AUTH_DB_PASSWORD:?Set AUTH_DB_PASSWORD}"
: "${LEDGER_DB_PASSWORD:?Set LEDGER_DB_PASSWORD}"
: "${AUTH_MIGRATION_PASSWORD:?Set AUTH_MIGRATION_PASSWORD}"
: "${LEDGER_MIGRATION_PASSWORD:?Set LEDGER_MIGRATION_PASSWORD}"
psql -X -v ON_ERROR_STOP=1 -v ECHO_ERRORS=off --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<'SQL'
\getenv auth_password AUTH_DB_PASSWORD
\getenv ledger_password LEDGER_DB_PASSWORD
\getenv auth_migration_password AUTH_MIGRATION_PASSWORD
\getenv ledger_migration_password LEDGER_MIGRATION_PASSWORD
BEGIN;
-- Do not write credentials or failed statement parameters to the server log.
SET LOCAL log_statement = 'none';
SET LOCAL log_min_error_statement = 'panic';
SET LOCAL log_parameter_max_length_on_error = 0;
DO $$
BEGIN
 IF EXISTS (
  SELECT FROM pg_namespace n JOIN pg_roles r ON r.oid=n.nspowner
  WHERE (n.nspname='auth' AND r.rolname<>'auth_migrator')
     OR (n.nspname='ledger' AND r.rolname<>'ledger_migrator')
 ) THEN
  RAISE EXCEPTION 'Existing service schemas must be owned by their migration roles; review ownership before running roles.sh';
 END IF;
 IF EXISTS (
  SELECT FROM pg_auth_members m JOIN pg_roles r ON r.oid=m.member
  WHERE r.rolname IN ('auth_app','ledger_app','auth_migrator','ledger_migrator')
 ) THEN
  RAISE EXCEPTION 'Service roles must not inherit other roles';
 END IF;
END $$;
SELECT 'CREATE ROLE ' || quote_ident(name) || ' LOGIN'
FROM unnest(ARRAY['auth_app','ledger_app','auth_migrator','ledger_migrator']) AS name
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = name) \gexec
ALTER ROLE auth_app NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD :'auth_password';
ALTER ROLE ledger_app NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD :'ledger_password';
ALTER ROLE auth_migrator NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD :'auth_migration_password';
ALTER ROLE ledger_migrator NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD :'ledger_migration_password';
SELECT format('REVOKE ALL ON DATABASE %I FROM PUBLIC', current_database()) \gexec
SELECT format('GRANT CONNECT ON DATABASE %I TO auth_app, ledger_app, auth_migrator, ledger_migrator', current_database()) \gexec
REVOKE ALL ON SCHEMA public FROM PUBLIC;
CREATE SCHEMA IF NOT EXISTS auth AUTHORIZATION auth_migrator;
CREATE SCHEMA IF NOT EXISTS ledger AUTHORIZATION ledger_migrator;
REVOKE ALL ON SCHEMA auth, ledger FROM PUBLIC;
GRANT USAGE ON SCHEMA auth TO auth_app;
GRANT USAGE ON SCHEMA ledger TO ledger_app;
ALTER ROLE auth_app SET search_path = auth, pg_catalog;
ALTER ROLE auth_migrator SET search_path = auth, pg_catalog;
ALTER ROLE ledger_app SET search_path = ledger, pg_catalog;
ALTER ROLE ledger_migrator SET search_path = ledger, pg_catalog;
-- Existing installations used public. Moving each table also moves its owned
-- sequences and indexes, preserving IDs, data and Goose migration versions.
DO $$
DECLARE item record;
BEGIN
 FOR item IN SELECT * FROM (VALUES
  ('users','auth','auth_migrator'),
  ('goose_auth_version','auth','auth_migrator'),
  ('budgets','ledger','ledger_migrator'),
  ('transactions','ledger','ledger_migrator'),
  ('goose_ledger_version','ledger','ledger_migrator')
 ) AS tables(name, schema_name, owner_name)
 LOOP
  IF to_regclass(format('public.%I',item.name)) IS NOT NULL THEN
   EXECUTE format('ALTER TABLE public.%I SET SCHEMA %I',item.name,item.schema_name);
   EXECUTE format('ALTER TABLE %I.%I OWNER TO %I',item.schema_name,item.name,item.owner_name);
  END IF;
 END LOOP;
END $$;
COMMIT;
SQL
