-- Pre-create the `auth` schema that GoTrue migrates into on first boot.
-- GoTrue connects with search_path=auth and expects the schema to exist
-- before its own migration tooling tries to CREATE TABLE.
-- (supabase/postgres handles this in its baked-in init; plain postgres
-- needs us to do it. We switch to supabase/postgres in D5 when Vault
-- is actually needed.)
CREATE SCHEMA IF NOT EXISTS auth;

-- GoTrue migration 20240612123726_enable_rls_update_grants references a
-- `postgres` role and grants it SELECT on every auth.* table. That role
-- exists by default in supabase/postgres; plain postgres doesn't ship it.
-- Pre-create as a no-login role so the grants succeed without opening a
-- second login surface.
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'postgres') THEN
    CREATE ROLE postgres NOLOGIN;
  END IF;
END
$$;
