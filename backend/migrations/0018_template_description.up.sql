-- 0018_template_description.up.sql — multilingual template description (A34.4). Policy=Data: the
-- human-readable "what does this template do" copy is a per-locale JSONB map ({de,en,fr,…}) on the
-- row, consumed by the SPA via a locale→en→de→first fallback (design 34-i18n §description). This
-- OVERRIDES the older `description TEXT` sketch (design 33 §3) — JSONB carries all 9 locales at once.
-- Idempotent (IF NOT EXISTS) + re-runnable against the final schema (parity 0016). NOT NULL DEFAULT
-- '{}' so every existing row (and every non-description INSERT) is a valid empty map, never SQL NULL.
ALTER TABLE templates ADD COLUMN IF NOT EXISTS description JSONB NOT NULL DEFAULT '{}'::jsonb;
