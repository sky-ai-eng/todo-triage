-- +goose Up
-- Harden tf.current_user_id() + tf.current_org_id() against the GUC
-- "defined-but-empty" state that can leak between transactions.
--
-- The baseline helpers (202605100001) were:
--   SELECT NULLIF(current_setting('request.jwt.claims', true)::jsonb ->> 'sub', '')::uuid
--
-- `current_setting(name, true)` returns:
--   - NULL  when the GUC has never been set on this connection
--   - ''    when set_config(name, '', _) ran, OR when a prior `set_config(name, val, true)` rolled back on some
--           pgx-managed connections (the GUC stays "defined" but reads back as empty until cleared)
--   - real value when actively set
--
-- The pre-cast NULLIF handles the empty-string case at the ->> step,
-- but `''::jsonb` fails BEFORE it gets there. Any caller invoking
-- tf.current_user_id() on a recycled connection that previously hit
-- set_config + rollback erroneously errors with:
--   "invalid input syntax for type json (SQLSTATE 22P02)"
--
-- Fix: short-circuit on NULL OR empty-string BEFORE the jsonb cast.
-- New shape returns NULL cleanly for both "never set" and "set then
-- effectively cleared", matching the original intent.
--
-- Production effects:
--   - Boot-time / admin-pool callers that never set claims keep
--     getting NULL → COALESCE picks the org-owner fallback (correct).
--   - Request-path callers with real claims keep getting the user
--     UUID (correct).
--   - Tests + cross-test connection reuse stop hitting the
--     ''::jsonb pothole.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION tf.current_user_id() RETURNS UUID
LANGUAGE SQL STABLE
AS $$
  SELECT CASE
    WHEN current_setting('request.jwt.claims', true) IS NULL
      OR current_setting('request.jwt.claims', true) = ''
    THEN NULL
    ELSE NULLIF(current_setting('request.jwt.claims', true)::jsonb ->> 'sub', '')::uuid
  END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION tf.current_org_id() RETURNS UUID
LANGUAGE SQL STABLE
AS $$
  SELECT CASE
    WHEN current_setting('request.jwt.claims', true) IS NULL
      OR current_setting('request.jwt.claims', true) = ''
    THEN NULL
    ELSE NULLIF(current_setting('request.jwt.claims', true)::jsonb ->> 'org_id', '')::uuid
  END;
$$;
-- +goose StatementEnd

-- +goose Down
SELECT 'down not supported';
