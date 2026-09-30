# Private library indexer

See [private media operations](../operations/private-media.md) for setup,
publisher, original-seed provisioning, member-network protection and readiness.

The optional private library uses a separate Torznab feed from the DHT catalog.
Enable it only after configuring membership, the private tracker and the seed
client. A media-library entry becomes a release only after its torrent has been
created and registered with the seed client. A catalog import alone does not
make the file downloadable.

The browser uses your reverse proxy's SSO. Indexer applications use the member's
own API key, created after sign-in. Membership suspension and key revocation
deny subsequent searches, torrent downloads and private tracker announces.
Interactive SSO redirects must not intercept the Torznab or announce endpoints;
those routes enforce their own credentials.

## Prowlarr

Add two **Generic Torznab** indexers when you want both catalogs:

| Field | DHT catalog | Private library |
|---|---|---|
| Name | BitAgent DHT | BitAgent Private Library |
| URL | `https://bitagent.example.org/torznab` | `https://bitagent.example.org/torznab/private` |
| API Path | `/api` | `/api` |
| API Key | Your member API key | Your member API key |

Replace the example hostname with your instance's address. Use **Test**, then
**Save** for each indexer. Private capabilities advertise Movies (2000) and TV
(5000), with text, supported identifier and season/episode searches. Empty
searches return recent releases. The catalogs paginate independently; the feed
does not claim to combine their totals or ordering.

## Jackett

The example Cardigann definitions read the [private library](definitions/bitagent-private.yml)
and [DHT catalog](definitions/bitagent-dht.yml) Torznab XML APIs. Replace their
example `links` URL with your instance's base URL, install them in Jackett's
definition directory and enter your personal API key. These are contribution
candidates; shipping them here does not mean Jackett or Prowlarr have accepted
them into their built-in catalogs.

Use `.torrent` downloads by default, including in automation clients. The
metainfo carries the private flag before the client discovers peers. Personal
magnets are optional for clients that support this private-tracker workflow.
Private items expose a personalized `.torrent` download URL. The definition
must retain `download`, rather than synthesizing a magnet from an infohash. The
torrent contains `private=1` and a member-specific private announce credential.
DHT discoveries continue to use their existing public magnets.

## Metrics and access limits

An authenticated torrent download records that the member requested a release.
It is distinct from completing a download. Private tracker announces supply
transfer and seeding counters; client-reported counters can be inaccurate and
must not be presented as independently verified byte measurements. Public DHT
magnets do not identify who downloads from their public swarm.

Private torrents reduce redistribution through DHT and let the operator revoke
announce credentials. A copied, still-valid credential is a bearer credential;
it cannot be guaranteed unshareable. Private torrents cannot prevent an
authorized member from copying the underlying file. Revocation blocks future
tracker announces and authenticated downloads; already-connected peers may
continue transferring. Treat torrent files and
announce URLs as credentials, avoid publishing them in logs or support reports,
and rotate a credential when it is exposed.

Disable request URI/query logging for the Torznab, torrent download and private
announce routes at the proxy as well as the application. Ordinary access logs
can capture API keys or announce passkeys. Keep HTTP client debug logging off
when it could record credential-bearing URLs or responses.

Seeder readiness observes qBittorrent's completed, active state and expires when
verification becomes stale. A readiness probe does not rehash the files: cached
progress or fast-resume state is not a fresh byte verification. Publishing must
require the seed client's full content check, and changed or restored media
must be rechecked before it is advertised again.

## Catalog submission

Before requesting a built-in indexer, verify the final public domain, TLS,
membership process, key issuance, search and recent-release results, real seed
availability, download and revocation behavior from both applications. Use
synthetic fixtures in public contributions. Offer maintainer test access through
an agreed private channel; never attach credentials to an issue.

Jackett calls a site **private** when registration requires an invitation or is
closed; always-open account registration is **semi-private**. This site type is
separate from the torrent's `private=1` flag. Follow
[Jackett's request guide](https://github.com/Jackett/Jackett/wiki/How-to-request-a-new-tracker)
and [definition documentation](https://github.com/Jackett/Jackett/wiki/Definition-format).
Its API-first YAML path avoids scraping the browser UI.

[Prowlarr's indexer repository](https://github.com/Prowlarr/Indexers) synchronizes
Jackett definitions. Verify the definition against its current schema and test
the imported entry in Prowlarr before treating both catalogs as supported. See
[Prowlarr's contribution guide](https://github.com/Prowlarr/Indexers/blob/master/CONTRIBUTING.md).
