# Consumer visibility and retained data

Set `SERVING_EXCLUDE_ADULT=true` (YAML `serving.exclude_adult: true`) to exclude
known adult releases from GraphQL and Torznab consumer reads. The option
defaults to false. Explicit category or info-hash requests cannot override it.

Positive evidence is an XXX classification, attached metadata with
`adult=true`, or the shipped core classifier's precision-vetted strong native
keywords with positive video/image payload bytes. Single weak words and the
matcher's abstention pattern are not adult evidence. Unknown signals remain
unknown; this does not promise recognition of every unclassified adult release.

Filtering occurs before pagination, representative grouping, counts and facets.
Raw torrent/file and content metadata lookups use the same consumer boundary.
It does not delete torrent data, sources, files or common metadata, enqueue a
backfill, spend a model allowance, or change classifier deletion rules.
Disabling the option restores visibility of retained adult rows.

Source-complete junk quarantine snapshots are excluded from consumer torrent
reads, including after recrawl and after the review window expires. The stored
torrent hash/name, public status, files shape and each source hash must agree
with the marker. Source arrays may be empty when no origin observations exist.
This protection does not depend on the optional verdict-reader switch or adult setting. The
operator restore transaction removes its quarantine row and rematches the
torrent; ordinary restored releases become visible again. Internal processing
and operator recovery use their original unfiltered data access.

Legacy or incomplete quarantine records do not automatically gain this
visibility authority. They remain retained for operator review; independent
positive adult evidence still applies when the adult option is enabled.
Snapshot completeness establishes recovery provenance, not model correctness.
Review legacy eligibility impact before introducing a broader removal policy;
do not delete or restore an archive merely to change consumer visibility.

`bitagent_serving_config{setting="exclude_adult"}` reports the resolved option;
`bitagent_serving_config{setting="quarantine_exclusion"}` is 1. These gauges
and `quarantine_snapshot_binding_required=1` describe configuration and do not
certify classification accuracy or usage.
