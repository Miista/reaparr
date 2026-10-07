<p align="center">
  <img src="assets/icon-256.png" width="128" alt="Reaparr icon">
</p>

# Reaparr

*Reaps your media once you're done with it.*

Reaparr watches Jellyfin on a schedule and, once a title has actually been
finished and enough time has passed, deletes it via Radarr/Sonarr — for
households that watch content once and don't build a collection, instead of
accumulating a library indefinitely like a typical *arr setup assumes.

## How it works

Reaparr's *matching* is entirely stateless — nothing about a previous sweep
is remembered, and every sweep independently re-derives "what's due" from
live Jellyfin data. Restarting the container is always a safe, complete
reset of that matching logic.

Settings and connections (see "Configuration" and "Dashboard" below) are the
one thing Reaparr does persist, in a small JSON file under its data
directory — so a value entered through the dashboard survives a restart.
This is unrelated to the sweep's statelessness: it's configuration, not a
memory of past deletions.

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

**This guarantee depends entirely on Radarr/Sonarr actually being
configured to hardlink (or copy) imports, not move them.** If Radarr/Sonarr
moves the file into place instead, there is no separate downloads-side
copy left — deleting the library file deletes the only copy of the data,
breaking the active seed and, on private trackers, potentially violating
minimum seed-time/ratio rules mid-count. Reaparr checks each configured
service's `copyUsingHardlinks` setting at the start of every sweep. If a
service's setting is off, Reaparr skips all deletions for that service this
sweep (movies for Radarr, episodes for Sonarr) and logs a clear error,
while the other service — if correctly configured — proceeds normally; a
movies-only misconfiguration doesn't block TV cleanup that's actually
safe, and vice versa. It's stateless, so there's no downside to skipping a
sweep — a fix takes effect on the very next sweep, no restart needed.
Enable "Use Hardlinks instead of Copy" in Radarr/Sonarr's Media Management
settings to resolve it. (This check reflects the setting used for normal
automatic imports; Radarr/Sonarr's Manual Import feature defaults to Move
mode regardless of
this setting — a documented upstream quirk Reaparr can't detect or
control, but which only applies to that one deliberate, human-triggered
action, not Reaparr's ongoing cleanup.)

## Seerr cleanup

Reaparr also cleans up a gap in Seerr's own
"Media Availability Sync" job: when a title's file is deleted (by Reaparr
or anything else), that job correctly marks the title's media record as
deleted, but leaves the associated request record behind — it just sits
there, stale, forever. Every sweep, Reaparr independently asks Seerr which
requests currently point at deleted media and deletes those media records
(which cascades to their requests) — a query, not something triggered by
Reaparr's own deletions, so it cleans up regardless of what deleted the
title and naturally retries on the next sweep if a delete call fails.

## Requirements

- Jellyfin, Radarr and/or Sonarr, and Seerr already set up and reachable on
  the same network as Reaparr. Sonarr is only needed if you have TV
  libraries; Radarr only if you have movie libraries — but at least one of
  the two is required.
- A Jellyfin API key: **Dashboard → API Keys → +** in the Jellyfin admin UI.
- A Radarr/Sonarr API key each: **Settings → General → Security → API Key**
  in their respective UIs.
- A Seerr API key: **Settings → General → API Key** in Seerr.

Until Jellyfin, Radarr or Sonarr, and Seerr each have both a URL and an API
key configured, Reaparr refuses to sweep: it logs which services are
missing, and the dashboard shows a "not running" banner naming them. The
dashboard itself stays up so you can configure them there.
- Developed and tested against Jellyfin 10.11.x. Jellyfin's Activity Log
  endpoint has no server-side event-type or date-range filter on this
  version — Reaparr fetches and filters it client-side, which is fine at
  household scale but worth knowing if you're auditing API traffic.

## Configuration

Every setting below can be set either as an environment variable or from
the dashboard (see "Dashboard" below) — whichever you prefer. **If an
environment variable is set, it always wins**: the dashboard shows that
field locked/read-only, naming the env var controlling it, and the API
rejects any attempt to change it. A field with no env var set is editable
from the dashboard and persisted to Reaparr's data directory, surviving a
container restart.

| Variable | Default | Description |
|---|---|---|
| `JELLYFIN_URL` | `http://jellyfin:8096` | Jellyfin base URL |
| `JELLYFIN_API_KEY` | — | Jellyfin API key. Required for sweeps to run (see "Requirements"), but not at container startup |
| `RADARR_URL` | `http://radarr:7878` | Radarr base URL |
| `RADARR_API_KEY` | — | Radarr API key. At least one of `RADARR_API_KEY`/`SONARR_API_KEY` is needed; either alone is enough for a movies-only or TV-only setup |
| `SONARR_URL` | `http://sonarr:8989` | Sonarr base URL |
| `SONARR_API_KEY` | — | Sonarr API key. See `RADARR_API_KEY` above |
| `DELETE_MOVIES_AFTER` | `7d` | How long after the last `VideoPlaybackStopped` event a still-played movie must wait before deletion (the grace period — see below) |
| `DELETE_TV_SHOWS_AFTER` | `7d` | Same, for TV episodes — configured independently of `DELETE_MOVIES_AFTER`, e.g. a shorter grace period for movies (single-sitting watches) and a longer one for TV (a season pack might sit half-watched between episodes for a while) |
| `POLL_SCHEDULE` | `@hourly` | Cron expression or descriptor (`@hourly`, `@daily`, `0 */6 * * *`, ...) for how often to sweep |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error` |
| `SEERR_URL` | `http://seerr:5055` | Seerr base URL — see "Seerr cleanup" above |
| `SEERR_API_KEY` | — | Seerr API key. Required for sweeps to run |
| `REAPARR_DATA_DIR` | `/app/data` | Where Reaparr persists dashboard-entered settings/connections (`config.json`) |

Both grace-period variables accept Go duration strings (`45m`, `6h`, `168h`,
`1h30m`) plus `d` (days) and `w` (weeks) suffixes — e.g. `7d`, `2w`.
Fractional day/week values are allowed (e.g. `1.5d`). Months are
deliberately unsupported since they aren't a fixed length.

The grace period exists to protect against a premature or mistaken "played"
flag (e.g. skipping credits) triggering deletion before anyone notices
something's wrong.

API keys are never logged in full, even at debug level, and are masked in
the dashboard/API once saved (only the last 4 characters shown) — only a
present/absent flag or masked value is ever exposed.

**Prefer setting API keys via environment variables.** Values from env
vars are only ever held in memory — they are never written to disk. A key
entered in the dashboard, by contrast, is stored in plain text in
`config.json` (file mode `0600`). You can mix the two freely, e.g. set
`SEERR_URL` via env and enter only the Seerr API key in the dashboard.

## Dashboard

Reaparr serves a small web dashboard on **port 8767** with three tabs:

- **Due for deletion** — a preview of everything currently matching the
  watched-and-past-grace-period rule, using the exact same fetch and
  matching code the scheduled sweep uses (not a separate,
  potentially-diverging check). The preview is held in memory only and
  rebuilt on startup, every 15 minutes, and after any dashboard save —
  independently of the sweep, which always does its own fresh check before
  deleting. Each item can be deleted immediately via its own button, which
  re-verifies that item live first, then calls the same Radarr/Sonarr
  delete path the cron sweep uses.
- **Settings** — the grace periods, poll schedule, and log level, editable
  unless locked by an env var (see "Configuration" above).
- **Connections** — Jellyfin/Radarr/Sonarr/Seerr URL + API key, each with a
  "Test connection" button, editable unless locked by an env var.

A settings/connections change made in the dashboard takes effect on the
very next sweep — no restart required.

**No authentication.** The dashboard has no login and is not meant to be
exposed outside your LAN — anyone who can reach port 8767 can view API keys
(masked) and trigger deletions. This matches the trust model this project
is typically deployed alongside (e.g. Maintainerr, qBittorrent's WebUI) but
is a real, known gap for a tool that can delete media; adding auth is a
planned follow-up, not yet implemented. Keep this port off any
internet-facing reverse proxy until it is.

## Deployment

Reaparr is a single static binary that both runs the scheduled sweep and
serves the dashboard on port 8767. It persists dashboard-entered
configuration under `/app/data` (see `REAPARR_DATA_DIR` above) — mount a
volume there if you want settings entered via the dashboard to survive a
container recreate (not just a restart, which the container's own
filesystem already survives). It needs network access to Jellyfin and
Radarr/Sonarr, and must **not** be given access to qBittorrent or its
network.

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
    DELETE_MOVIES_AFTER: 2d
    DELETE_TV_SHOWS_AFTER: 7d
    POLL_SCHEDULE: "@hourly"
  ports:
    - 8767:8767
  volumes:
    - ./reaparr/data:/app/data
  networks:
    - media
```

Every `environment:` line above is optional — omit any of them (or all of
them) and configure that field from the dashboard instead. The example
shows the fully-env-var-driven setup for parity with Reaparr's original,
pre-dashboard deployment style.

## Development

```sh
go test ./... -race
go build .
docker build -t reaparr .
```

## See also

If you want a fuller rule engine (age, size, rating, request status, and
more — not just watched state) with a UI, or need Plex/Emby support, look at
[Maintainerr](https://github.com/Maintainerr/Maintainerr) instead. It's a
heavier, stateful tool with a database and its own collection/approval
workflow, and — unlike Reaparr — it can act on the download client directly
rather than staying strictly decoupled from it. Reaparr is deliberately
smaller in scope: Jellyfin-only, stateless, one policy (watched + grace
period), and it will never touch qBittorrent.

## License

[MIT](LICENSE)
