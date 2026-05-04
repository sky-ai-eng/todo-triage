-- Per-project Curator spec-authorship skill (SKY-221).
--
-- The Curator's second focus (after curating project knowledge) is
-- authoring well-specced tickets. The "well-specced" definition is
-- per-project: a memory team's tickets read differently than an
-- infrastructure team's tickets. We model that as a pointer from
-- the project to a prompt — the prompt body becomes a literal
-- Claude Code skill file (SKILL.md) in the Curator session's cwd
-- on each dispatch.
--
-- Nullable: NULL means "use the seeded system default"
-- (`system-ticket-spec`). Project create defaults to that ID when
-- the client doesn't pass anything; users override per-project via
-- the Projects page. ON DELETE SET NULL on the prompts FK so a
-- deleted prompt falls back to the default rather than leaving a
-- dangling pointer.
ALTER TABLE projects
    ADD COLUMN spec_authorship_prompt_id TEXT
        REFERENCES prompts(id) ON DELETE SET NULL;
