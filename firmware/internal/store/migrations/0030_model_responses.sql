-- Responses is an independent text protocol surface. Preserve existing access.
ALTER TABLE models ADD COLUMN entry_responses INTEGER NOT NULL DEFAULT 1;
UPDATE models SET entry_responses = entry_openai;
