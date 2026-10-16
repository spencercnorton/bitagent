package junkpurge

const quarantineExpireTombstoneSQL = `
UPDATE junkpurge_quarantine SET expired_at = now()
WHERE info_hash = ANY($1) AND expired_at IS NULL`
