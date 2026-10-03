"""Release kinds for the opt-in SQLite library, separate from DHT content."""

TV_KINDS = frozenset({"episode", "season", "show"})
KINDS = TV_KINDS | {"movie", "music", "generic"}
CATEGORY_KINDS = {
    2000: frozenset({"movie"}),
    3000: frozenset({"music"}),
    5000: TV_KINDS,
    8000: frozenset({"generic"}),
}
CATEGORY_BY_KIND = {kind: category for category, kinds in CATEGORY_KINDS.items() for kind in kinds}
