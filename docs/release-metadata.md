# Release-name metadata claims

Structured resolution, source, codec and release-group values describe explicit
release-name evidence. They do not verify the media's streams or tracks. Keep
the original release name; clients can inspect and parse it independently.
Absent or contradictory evidence remains unknown.

The codec parser recognizes H.264/AVC, H.265/HEVC and AV1 with bounded token
separators. `HEVC` and `AV1` are additional codec values. Existing serialized
values remain unchanged, including `H264`, `x264`, `x265`, `XviD`, `DivX`,
`MPEG2` and `MPEG4`. An explicit `x265` name token retains the legacy `x265`
value; an HEVC/H.265 token does not claim that encoder. Contradictory codec
families produce no structured codec value.

A trailing scene group can be recognized after codec, audio or source evidence,
including a dotted codec or audio/channel suffix. Group parsing is independent
of codec recognition. Bare hyphenated titles, technical labels and revision
tokens do not become groups. Curated leading fansub-group handling remains in
the anime classifier.

GraphQL, JSON and Torznab expose these codec/group claims with the original
release name preserved. Nullable values stay nullable. GraphQL codec enums are
additive; clients with exhaustive codec lists should accept unknown future
values rather than reject the entire result.

There is no database schema migration or automatic historical repair in this
change. New parser behavior applies on classification. Before a field-only
repair, freeze old/new values and their source/provenance and preserve manual
overrides. Do not run the full classifier as a metadata repair.

Readers must understand `HEVC` and `AV1` before those values are persisted.
An older binary whose codec scanner rejects those values needs a compatible
reader, the saved field rollback, or a matching database backup. Disabling
new inference alone does not remove already stored codec values.
