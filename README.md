# Purgarr

Polls Jellyfin for watched titles and, after a grace period, deletes them via
Radarr/Sonarr — for households that watch content once and don't build a
collection, instead of accumulating a library indefinitely like a typical
*arr setup assumes.

## How it works

On a configurable schedule, Purgarr:

1. Queries every Jellyfin user account for played movies/episodes
   (`/Users/{id}/Items?Filters=IsPlayed`). Any one account having watched a
   title is enough to mark it for cleanup — this doesn't require every
   account on the server to have watched it.
2. Records each title's watched timestamp (Jellyfin's own
   `LastPlayedDate`) in a local JSON file, keyed by movie/series ID.
3. Once a title has been watched for longer than the configured grace
   period, deletes it via the Radarr or Sonarr API (`deleteFiles=true`).

TV shows are cleaned up at the series level — a season pack is treated as
one unit, matching how Sonarr/qBittorrent already track it as a single
release.

## What it will never do

Purgarr only ever talks to Jellyfin (read-only) and Radarr/Sonarr (delete).
It is never given qBittorrent credentials or network access, and it never
touches qBittorrent directly. Because Radarr/Sonarr import via hardlink,
deleting the Radarr/Sonarr-side copy only drops one of two links — the
downloads-side copy and its seed continue independently, until
qBittorrent's own seeding-limit policy removes it on its own schedule. The
two cleanup paths stay fully decoupled by design.

## Configuration

All configuration is via environment variables.

| Variable | Default | Description |
|---|---|---|
| `JELLYFIN_URL` | `http://jellyfin:8096` | Jellyfin base URL |
| `JELLYFIN_API_KEY` | — (required) | Jellyfin API key |
| `RADARR_URL` | `http://radarr:7878` | Radarr base URL |
| `RADARR_API_KEY` | — (required) | Radarr API key |
| `SONARR_URL` | `http://sonarr:8989` | Sonarr base URL |
| `SONARR_API_KEY` | — (required) | Sonarr API key |
| `GRACE_PERIOD_DAYS` | `7` | Days after watching before a title is deleted |
| `POLL_SCHEDULE` | `@hourly` | Cron expression or descriptor (`@hourly`, `@daily`, `0 */6 * * *`, ...) for how often to poll Jellyfin and sweep for due deletions |
| `STORE_PATH` | `/data/purgarr.json` | Path to the persisted watched-state file |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error` |

The grace period exists to protect against a premature or mistaken
"played" flag (e.g. skipping credits) triggering deletion before anyone
notices something's wrong.

## Deployment

Purgarr is a single static binary with no HTTP surface — it's a background
process only, no ports to expose. It needs network access to Jellyfin and
Radarr/Sonarr, and a volume mount for `STORE_PATH` so watched-state
survives restarts. It must **not** be given access to qBittorrent or its
network, per the design above.

```yaml
purgarr:
  build: ./purgarr
  environment:
    JELLYFIN_URL: http://jellyfin:8096
    JELLYFIN_API_KEY: ${JELLYFIN_API_KEY}
    RADARR_API_KEY: ${RADARR_API_KEY}
    SONARR_API_KEY: ${SONARR_API_KEY}
    GRACE_PERIOD_DAYS: "7"
  volumes:
    - ./purgarr/data:/data
  networks:
    - media
```

## Development

```sh
go test ./... -race
go build .
docker build -t purgarr .
```

See `plan.md` for the original design rationale and decisions.
