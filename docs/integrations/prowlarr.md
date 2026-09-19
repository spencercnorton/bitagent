# Prowlarr integration

BitAgent is added to Prowlarr as a **Generic Torznab** indexer; Prowlarr then syncs it to every connected *arr application.

## Add the indexer

1. **Open Prowlarr → Indexers → Add Indexer → Generic Torznab**.
2. Fill in:

   | Field | Value |
   |---|---|
   | Name | `BitAgent` |
   | Enable | ✓ |
   | URL | `https://bitagent.example.com/torznab` (or compose service `http://bitagent:3333/torznab`) |
   | API Key | your `TORZNAB_API_KEY` |
   | API Path | `/api` (Prowlarr default; leave it) |
   | Categories | leave empty — Prowlarr negotiates via the caps endpoint |
   | Indexer Tags | optional |
   | Tracked | ✓ |
   | Priority | `25` |

3. Click **Test**. Green = success.
4. Save.

`[screenshot: prowlarr-add-generic-torznab]`

Prowlarr will sync the indexer to all connected *arr applications (Sonarr/Radarr/Lidarr/Readarr) automatically. You don't need to add BitAgent in each *arr separately — Prowlarr is the one place you configure it.

## Categories

Prowlarr negotiates categories via the Torznab `?t=caps` endpoint. BitAgent's caps declare:

| Newznab ID | Slug |
|---|---|
| 5000 / 5030 / 5040 | TV / TV/SD / TV/HD |
| 2000 / 2030 / 2040 / 2050 | Movies / Movies/SD / Movies/HD / Movies/UHD |
| 3000 / 3010 / 3020 | Audio / Audio/MP3 / Audio/Lossless |
| 7000 / 7020 | Books / Books/EBook |

Prowlarr's per-app sync respects these category mappings. Sonarr only receives TV-categorised results, Radarr only movies, etc.

## Troubleshooting

**Test fails with "Indexer doesn't support tv-search"**
Your `?t=caps` response is malformed or empty. Run `curl https://bitagent.example.com/torznab?t=caps&apikey=$TORZNAB_API_KEY` and verify you get valid XML with a `<searching>` block. If empty, BitAgent hasn't bootstrapped yet — wait the full 3 minutes after compose-up.

**Search returns results in Prowlarr but Sonarr/Radarr show empty**
Prowlarr filters categories per-app. Verify your Sonarr instance is mapped in Prowlarr's *App Sync* with the right categories selected (5000-series for Sonarr, 2000-series for Radarr).

**Indexer shows "Failing" after working for a while**
Prowlarr health-pings each indexer. If BitAgent restarts during a ping, it can mark it failing. Click **Test** to clear; it'll go back to green within 30s.

`[screenshot: prowlarr-app-sync]`
