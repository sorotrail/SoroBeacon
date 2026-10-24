-- Free-form tags on monitors for grouping and filtering (SQLite version).
--
-- SQLite has no array type, so tags are stored as a JSON array of strings,
-- exactly like contract_ids. Filtering uses instr() over the quoted
-- JSON text: a tag can only appear as its own quoted element, so the
-- substring search is an exact tag match with no LIKE wildcards to escape.

ALTER TABLE monitors ADD COLUMN tags TEXT NOT NULL DEFAULT '[]';
