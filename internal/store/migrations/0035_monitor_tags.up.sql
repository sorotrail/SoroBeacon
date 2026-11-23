-- Free-form tags on monitors for grouping and filtering.
--
-- A deployment with more than a dozen monitors has no way to separate
-- "treasury" monitors from "protocol health" ones without a folder
-- hierarchy. Tags are the cheapest structure that does not impose one.
--
-- Design decisions:
--   * text[] rather than a join table: the tag set per monitor is small and
--     bounded at the API (at most 25 tags, each at most 64 characters), and
--     every query is "monitors carrying this tag", which a containment
--     operator answers directly.
--   * A GIN index, which is the index type array containment (@>) is designed
--     to use; a B-tree cannot serve `tags @> ...` and would leave every tag
--     filter a sequential scan.
--   * Tags are normalised (trimmed, lowercased, deduplicated) in the store
--     before they are written, so "Treasury" and "treasury" are one tag.
--   * NOT NULL DEFAULT '{}' so a monitor created before the migration has an
--     empty tag list rather than a NULL that every read must special-case.

ALTER TABLE monitors ADD COLUMN tags TEXT[] NOT NULL DEFAULT '{}';

CREATE INDEX monitors_tags_gin_idx ON monitors USING GIN (tags);
