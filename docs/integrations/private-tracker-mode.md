# Authenticated indexing and private media torrents

BitAgent supports two different access patterns. Protecting an indexer's search
API with a key controls who can query that API. It does not make the indexed
torrents private.

## Authenticated DHT gateway

The DHT catalog discovers public torrents and returns their public magnets. The
core Torznab endpoint can require `TORZNAB_API_KEY` or `TORZNAB_API_KEYS`. The
web service's Torznab proxy uses a personal member API key and forwards requests
to the core with its separate operator credential.

Public torrents continue to use their public swarms. API authentication does not
provide private tracker announces, user seed ratios, or protection against
redistributing those public magnets. See [Prowlarr setup](prowlarr.md) and the
[Torznab reference](../reference/torznab-api.md) for configuration.

## Private media library

The optional private library publishes separate media releases with
`private=1` metainfo, approved SSO memberships and personalized private tracker
credentials. It has its own feed at `/torznab/private/api`. Tracker observations
provide per-member transfer and seeding metrics; link requests and completed
download events remain separate measurements.

Use authenticated `.torrent` downloads by default. They carry the private flag
and the member's announce credential. Personal magnets are an optional
convenience for compatible clients and also contain a bearer credential. Copying
a valid credential can give another person access; neither private torrents nor
authentication can guarantee that an authorized member will not redistribute a
file. Revocation blocks future authenticated requests and tracker announces,
but does not disconnect transfers that peers have already established.

See [Private library indexer](private-indexer.md) for setup, separate catalog
definitions, metrics limits and upstream submission requirements. A deployment
with invite-only membership can be listed as a private indexer; always-open
account registration is classified as semi-private by Jackett. This catalog
classification is separate from the torrent's `private=1` flag.
