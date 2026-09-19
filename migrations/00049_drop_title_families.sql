-- +goose Up
-- +goose StatementBegin

-- Drop the title-family registry. Package familydb described itself as
-- "work item F1 ... F1 is inert: nothing consumes these tables yet", and the
-- F6 decision policy that was to consume it was never written. Verified
-- 2026-08-23 on production: all four tables hold 0 rows, so this drops
-- nothing that was ever built. The Go package, its fx wiring and the
-- family-rebuild CLI go with it.
--
-- Reversal is 00035_title_families.sql, unchanged in history.

drop table if exists title_family_lookup;
drop table if exists title_family_tokens;
drop table if exists title_family_members;
drop table if exists title_families;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Deliberately not reversible here: recreating empty tables for a deleted
-- package would restore the confusion, not the capability. Re-apply
-- 00035_title_families.sql if the registry is ever revived.
select 1;

-- +goose StatementEnd
