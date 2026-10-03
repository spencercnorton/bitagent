# Prowlarr integration

BitAgent isn't in Prowlarr's built-in indexer list. Don't pick the **BitMagnet (Local DHT)** entry there: it has no API key field, so it can't connect to a BitAgent that requires a key. Add BitAgent as a **Generic Torznab** indexer instead. It takes any address, including another machine, a VPN address or an https reverse proxy, and Prowlarr syncs it to every connected \*arr application.

## Add the indexer

1. In Prowlarr, open **Indexers → Add Indexer** and choose **Generic Torznab**.
2. Fill in:

   | Field | Value |
   |---|---|
   | Name | `BitAgent` |
   | Url | the address Prowlarr reaches BitAgent at, followed by `/torznab` (see below) |
   | API Path | `/api` (the default; leave it) |
   | API Key | your `TORZNAB_API_KEY`, or one of the keys in `TORZNAB_API_KEYS`; leave it empty if neither is set |

   | Where BitAgent runs | Url |
   |---|---|
   | Same Docker network as Prowlarr (the example compose service) | `http://bitagent:3333/torznab` |
   | Same machine, Prowlarr not in Docker | `http://localhost:3333/torznab` |
   | Another machine, or over a VPN | `http://<its IP or hostname>:3333/torznab` |
   | Behind a reverse proxy | `https://bitagent.example.com/torznab` |

3. Click **Test**, then **Save**.

Prowlarr reads BitAgent's categories and search options from its caps endpoint, so there is nothing else to set. It then syncs the indexer to every connected \*arr application (Sonarr, Radarr, Lidarr, Readarr), so you don't add BitAgent to each one.

## Categories

| ID | Category | Subcategories |
|---|---|---|
| 2000 | Movies | 2030 SD, 2040 HD, 2045 UHD, 2060 3D |
| 3000 | Audio | 3030 Audiobook |
| 4000 | PC | 4050 Games |
| 5000 | TV | 5030 SD, 5040 HD, 5045 UHD, 5070 Anime |
| 6000 | XXX | 6070 Other |
| 7000 | Books | 7020 EBook, 7030 Comics |
| 8000 | Other | |

Anime series come back under both TV (5000) and TV/Anime (5070), so they match Prowlarr's **Anime Sync Categories** for Sonarr as well as ordinary TV searches.

## Troubleshooting

**Test fails with "HTTP request failed: [401:Unauthorized]"**
The API key is missing or wrong. It must match `TORZNAB_API_KEY`, or one of the keys in `TORZNAB_API_KEYS`, on the BitAgent instance.

**Test fails with any other error**
From the machine Prowlarr runs on, request the caps endpoint with the same address:
`curl "http://bitagent:3333/torznab/api?t=caps&apikey=$TORZNAB_API_KEY"`
It should return XML with a `<searching>` block. If the connection fails, the address is wrong or BitAgent isn't running.

**Results show in Prowlarr but not in Sonarr or Radarr**
Prowlarr only syncs an indexer to an app when it serves that app's categories. In Prowlarr, open **Settings → Apps**, edit the app and check its **Sync Categories** (5000-series for Sonarr, 2000-series for Radarr).

**The indexer shows as failing after working for a while**
When BitAgent can't be reached, during a restart for example, Prowlarr pauses the indexer ("Indexer is disabled till … due to recent failures"). Once BitAgent is back, click **Test** to clear it.
