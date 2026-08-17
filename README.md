# Reaparr

Reaparr watches Jellyfin on a schedule and, once a title has actually been
finished and enough time has passed, deletes it via Radarr/Sonarr — for
households that watch content once and don't build a collection, instead of
accumulating a library indefinitely like a typical *arr setup assumes.

## How it works

Reaparr is entirely stateless — it keeps no store, no persisted file, and no
memory of any previous sweep. Every sweep is a fresh, complete pass over live
Jellyfin data. Restarting the container is a safe, complete reset.

On a configurable cron schedule, each sweep:

1. Asks Jellyfin's Activity Log for every item whose most recent
   `VideoPlaybackStopped` event is older than the grace period (set A).
   `VideoPlaybackStopped` fires on any stop, including someone quitting
   partway through, so this alone doesn't mean "finished."
2. Asks every Jellyfin user account for the movies/episodes they currently
   have marked `Played=true` (set B). Any one account having watched a title
   is enough — it doesn't require every account on the server to agree,
   since in a multi-user household requiring everyone to finish before
   cleanup would mean most titles never qualify at all.
3. Acts only on the intersection, A ∩ B: items that are both currently fully
   played AND stopped playing a while ago. Because B is re-checked live every
   sweep, a title that gets unplayed again (started, abandoned, `Played`
   flips back to false) simply stops appearing in B and is never touched —
   there is nothing stored to go stale.

For each item in the intersection, Reaparr resolves it to Radarr/Sonarr's own
internal ID before deleting:

- **Movies** resolve to Radarr via the item's TMDB ID.
- **Episodes** resolve to Sonarr via the *parent series'* TVDB ID (not the
  episode's own TVDB ID) — Sonarr tracks and deletes at the series level, so
  a season pack is treated as one unit, matching how Sonarr already tracks
  it as a single release.

If an item can't be matched into Radarr or Sonarr (e.g. missing provider ID,
or Radarr/Sonarr simply doesn't track that title), it's logged as a warning
and skipped — not retried as an error. A genuine lookup or delete failure
(e.g. Radarr/Sonarr unreachable) is logged as an error and naturally
retried on the next sweep, since there's no state marking it as "handled."

## What it will never do

Reaparr only ever talks to Jellyfin (read-only) and Radarr/Sonarr (delete).
It is never given qBittorrent credentials or network access, and never
touches qBittorrent or Jellyfin's own library directly.

Radarr/Sonarr import media via hardlink, so deleting the Radarr/Sonarr-side
file only drops one of two hardlinks — the downloads-side copy and its
ongoing seed are left completely untouched, continuing independently until
qBittorrent's own seeding-limit policy removes it on its own schedule. The
two cleanup paths stay fully decoupled by design.

## Requirements

- Jellyfin, Radarr, and/or Sonarr already set up and reachable on the same
  network as Reaparr. Sonarr is only needed if you have TV libraries;
  Radarr only if you have movie libraries.
- A Jellyfin API key: **Dashboard → API Keys → +** in the Jellyfin admin UI.
- A Radarr/Sonarr API key each: **Settings → General → Security → API Key**
  in their respective UIs.
- Developed and tested against Jellyfin 10.11.x. Jellyfin's Activity Log
  endpoint has no server-side event-type or date-range filter on this
  version — Reaparr fetches and filters it client-side, which is fine at
  household scale but worth knowing if you're auditing API traffic.

## Configuration

All configuration is via environment variables.

| Variable | Default | Description |
|---|---|---|
| `JELLYFIN_URL` | `http://jellyfin:8096` | Jellyfin base URL |
| `JELLYFIN_API_KEY` | — (required) | Jellyfin API key |
| `RADARR_URL` | `http://radarr:7878` | Radarr base URL |
| `RADARR_API_KEY` | — | Radarr API key. At least one of `RADARR_API_KEY`/`SONARR_API_KEY` is required; either alone is enough for a movies-only or TV-only setup |
| `SONARR_URL` | `http://sonarr:8989` | Sonarr base URL |
| `SONARR_API_KEY` | — | Sonarr API key. See `RADARR_API_KEY` above |
| `GRACE_PERIOD` | `7d` | How long after the last `VideoPlaybackStopped` event a still-played title must wait before deletion. Accepts Go duration strings (`45m`, `6h`, `168h`, `1h30m`) plus `d` (days) and `w` (weeks) suffixes — e.g. `7d`, `2w`. Fractional day/week values are allowed (e.g. `1.5d`). Months are deliberately unsupported since they aren't a fixed length. |
| `POLL_SCHEDULE` | `@hourly` | Cron expression or descriptor (`@hourly`, `@daily`, `0 */6 * * *`, ...) for how often to sweep |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error` |

The grace period exists to protect against a premature or mistaken "played"
flag (e.g. skipping credits) triggering deletion before anyone notices
something's wrong.

API keys are never logged in full, even at debug level — only a
present/absent flag.

## Deployment

Reaparr is a single static binary with no HTTP surface and no persisted
state — it's a background process only, with no ports to expose and no
volumes to mount. It needs network access to Jellyfin and Radarr/Sonarr, and
must **not** be given access to qBittorrent or its network.

Pre-built images are published to
[ghcr.io/miista/reaparr](https://github.com/Miista/reaparr/pkgs/container/reaparr)
for `linux/amd64` and `linux/arm64`.

```yaml
reaparr:
  image: ghcr.io/miista/reaparr:latest
  restart: unless-stopped
  environment:
    JELLYFIN_URL: http://jellyfin:8096
    JELLYFIN_API_KEY: ${JELLYFIN_API_KEY}
    RADARR_URL: http://radarr:7878
    RADARR_API_KEY: ${RADARR_API_KEY}
    SONARR_URL: http://sonarr:8989
    SONARR_API_KEY: ${SONARR_API_KEY}
    GRACE_PERIOD: 7d
    POLL_SCHEDULE: "@hourly"
  networks:
    - media
```

## Development

```sh
go test ./... -race
go build .
docker build -t reaparr .
```

## License

[MIT](LICENSE)
