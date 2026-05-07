-- SKY-220: add classifier metadata to entities.
--
-- classified_at: marker so the post-poll runner can distinguish
-- "never tried" from "tried, scored below threshold." Query is
-- `WHERE project_id IS NULL AND classified_at IS NULL` so re-polls
-- don't keep re-firing classification on entities that already lost
-- every project's vote. Nullable: existing rows pre-classifier have
-- NULL, which means the runner picks them up on its first cycle
-- after upgrade — that's the intended one-shot retro-classify
-- behavior on first launch with the classifier enabled.
--
-- classification_rationale: the highest-scoring project's one-sentence
-- rationale from its Haiku call, regardless of whether the score
-- crossed threshold. The valuable signal is the runner-up case —
-- "closest match was Auth Migration at 45/100, because: ..." tells
-- the user why an unassigned entity didn't get tagged. Surfaced in
-- the project-creation backfill popup (PR B) as a hover hint.
ALTER TABLE entities ADD COLUMN classified_at DATETIME;
ALTER TABLE entities ADD COLUMN classification_rationale TEXT;

-- Stamp every pre-existing entity as "we have a final answer" (NULL
-- project, no rationale) so the post-poll classifier doesn't try to
-- backfill the entire history on first launch with the classifier
-- enabled. Existing untagged entities stay untagged forever unless
-- the user reaches them via the project-creation backfill popup
-- (PR B). This is the deliberate "no retro-classification" stance:
-- nobody is using projects yet, so there's no real cost, and avoiding
-- a 200-entity classification stampede on first launch is worth a
-- one-line stamp.
UPDATE entities SET classified_at = CURRENT_TIMESTAMP WHERE classified_at IS NULL;
