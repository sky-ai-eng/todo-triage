-- Store per-prompt tool allowlist parsed from SKILL.md frontmatter
-- (allowed-tools:) or agent definition frontmatter (tools:). Merged
-- into --allowedTools at spawn time so MCP tools declared by skills
-- reach the headless claude process.
--
-- Format: comma-separated tool names/patterns, same shape as the
-- --allowedTools CLI flag. NOT NULL DEFAULT '' — empty string = no extras.

ALTER TABLE prompts ADD COLUMN allowed_tools TEXT NOT NULL DEFAULT '';
