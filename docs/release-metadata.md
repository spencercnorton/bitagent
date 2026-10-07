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

Codec/group recognition itself requires no schema migration. Richer claims use
the nullable column described below; neither change performs automatic historical
repair. New parser behavior applies on classification. Before a field-only
repair, freeze old/new values and their source/provenance and preserve manual
overrides. Do not run the full classifier as a metadata repair.

Readers must understand `HEVC` and `AV1` before those values are persisted.
An older binary whose codec scanner rejects those values needs a compatible
reader, the saved field rollback, or a matching database backup. Disabling
new inference alone does not remove already stored codec values.

Migration 57 adds nullable `release_attributes` JSONB. Version 1 retains the
parser identity and SHA-256 of the original release name with explicit HDR/DV,
audio format/channel/feature, encoder and revision claims. No supported claim
means SQL NULL. Multiple advertised formats can coexist; contradictory scalar
channel or encoder claims remain unknown. HEVC does not imply x265, and Dolby
Vision does not manufacture a codec, bit depth or verified track.

GraphQL `releaseAttributes` and its search input expose these advertised claims.
Torznab emits `claimedhdr`, `claimedaudio`, `claimedaudiochannels`,
`claimedaudiofeatures`, `claimedrevision` and `claimedencoder` only when present.
These optional extension fields keep their claim status explicit.

The migration must follow earlier migrations in the release lineage. Its
downgrade refuses to discard populated claim data. No startup-wide backfill is
performed; bounded field repair is separate from classification and must retain
source checks, previous values and override precedence.
