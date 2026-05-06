-- Surface bare-clone success/failure on each repo row so the Repos page
-- can flag failed clones without ad-hoc parsing of git stderr. Three
-- columns, populated whenever EnsureBareClone runs:
--   clone_status: 'pending' (default for legacy rows pre-first-attempt),
--                 'ok' on success, 'failed' on error
--   clone_error:  raw stderr / preflight output captured at failure time
--   clone_error_kind: 'ssh' when our PreflightSSH against the configured
--                     GitHub SSH host fails (config is using SSH AND the
--                     SSH side is the cause), 'other' otherwise; NULL
--                     while clone_status is 'pending' or 'ok'
--
-- Existing rows get NULL/default and will be flipped on the next
-- BootstrapBareClones pass.
ALTER TABLE repo_profiles ADD COLUMN clone_status TEXT NOT NULL DEFAULT 'pending';
ALTER TABLE repo_profiles ADD COLUMN clone_error TEXT;
ALTER TABLE repo_profiles ADD COLUMN clone_error_kind TEXT;
