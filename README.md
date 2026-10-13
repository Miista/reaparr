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
- **TV is judged per season, never per episode.** Played episodes are
  grouped by series and season, and the series is matched to Sonarr via its
  TVDB ID. A season is only eligible once **every** episode Sonarr tracks
  for it has aired, has a file, and is played in Jellyfin (an episode counts
  as played if any user played it), and the most recent stop event across
  the season's episodes is past the TV grace period. A season that is still
  airing, or has an episode Sonarr hasn't downloaded, is never eligible.
  Deleting a season first unmonitors it (so Sonarr won't re-download it),
  then deletes that season's episode files. The series and its other
  seasons are left alone.

If an item can't be matched into Radarr or Sonarr (e.g. missing provider ID,
or Radarr/Sonarr simply doesn't track that title), it's logged as a warning
and skipped — not retried as an error. The dashboard shows the reason on
that row. A genuine lookup or delete failure
(e.g. Radarr/Sonarr unreachable) is logged as an error and naturally
retried on the next sweep, since there's no state marking it as "handled."

## Keepers

A movie or series carrying the keep tag (`REAPARR_SETTING_KEEP_TAG`,
default `reaparr-keep`) in Radarr/Sonarr is **never deleted** — not by the
scheduled run, and not by the dashboard's delete buttons. Add the tag from
the dashboard (Reaparr creates it in Radarr/Sonarr the first time), or
yourself in Radarr/Sonarr:

- **Due** tab: press **Keep** on a row, or select rows and press **Keep N
  selected**.
- **Library** tab: a poster grid of every movie in Radarr and series in
  Sonarr — watched or not — with search and All / Kept / Movies / Series
  filters, and a **Keep** toggle under each poster. This is how you protect
  something before it's ever watched.

In Sonarr a tag applies to the whole series, so keeping one season keeps
every season. If Reaparr can't read the tag from Radarr/Sonarr, it deletes
nothing that run rather than risk deleting a keeper. The tag is unique to
Reaparr, so it doesn't change which indexers, profiles or download clients
apply to a title.

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

Every env var is named `REAPARR_SETTING_<KEY>`, where `<KEY>` is the
setting's key upper-cased, with `.` replaced by `_`.

| Variable | Default | Description |
|---|---|---|
| `REAPARR_SETTING_JELLYFIN_URL` | `http://jellyfin:8096` | Jellyfin base URL |
| `REAPARR_SETTING_JELLYFIN_API_KEY` | — | Jellyfin API key. Required for sweeps to run (see "Requirements"), but not at container startup |
| `REAPARR_SETTING_RADARR_URL` | `http://radarr:7878` | Radarr base URL |
| `REAPARR_SETTING_RADARR_API_KEY` | — | Radarr API key. At least one of Radarr/Sonarr is needed; either alone is enough for a movies-only or TV-only setup |
| `REAPARR_SETTING_SONARR_URL` | `http://sonarr:8989` | Sonarr base URL |
| `REAPARR_SETTING_SONARR_API_KEY` | — | Sonarr API key. See the Radarr API key above |
| `REAPARR_SETTING_SEERR_URL` | `http://seerr:5055` | Seerr base URL — see "Seerr cleanup" above |
| `REAPARR_SETTING_SEERR_API_KEY` | — | Seerr API key. Required for sweeps to run |
| `REAPARR_SETTING_MOVIES_GRACE_PERIOD` | `7d` | How long after the last `VideoPlaybackStopped` event a still-played movie must wait before deletion (the grace period — see below) |
| `REAPARR_SETTING_TV_GRACE_PERIOD` | `7d` | Same, for TV seasons, measured from the last stop of any episode in the season — configured independently of the movies grace period |
| `REAPARR_SETTING_POLL_SCHEDULE` | `@hourly` | Cron expression or descriptor (`@hourly`, `@daily`, `0 */6 * * *`, ...) for how often to sweep |
| `REAPARR_SETTING_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error` |
| `REAPARR_SETTING_KEEP_TAG` | `reaparr-keep` | Radarr/Sonarr tag marking keepers — see "Keepers" below |
| `REAPARR_SETTING_JELLYFIN_USERS` | — (everyone) | Comma-separated Jellyfin usernames or user IDs whose watched state counts — both their played items and their playback events. Any one listed user having watched something is enough. Empty means every user. If none of the listed users exist, sweeps are skipped rather than falling back to everyone. Also settable from the dashboard's Watchers card |
| `REAPARR_SETTING_DAEMON_ENABLED` | `true` | Whether the scheduled sweep deletes automatically. When `false`, nothing is deleted on schedule — the dashboard still lists what's ripe for deletion and you delete from there. An unparseable value counts as `false` |

Both grace-period variables accept Go duration strings (`36h`, `168h`,
`30h30m`) plus `d` (days) and `w` (weeks) suffixes — e.g. `7d`, `2w`.
Fractional day/week values are allowed (e.g. `1.5d`), but days/weeks can't
be combined with other units (`7d2h` — use `170h`). Months are
deliberately unsupported since they aren't a fixed length.

The grace period exists to protect against a premature or mistaken "played"
flag (e.g. skipping credits) triggering deletion before anyone notices
something's wrong, so it has a **minimum of 1 day**. The dashboard and API
reject anything shorter; a shorter env var value is raised to 1 day (and
logged as an error). It only governs the scheduled run — deleting by hand
from the dashboard works right away.

API keys are never logged in full, even at debug level, and are masked in
the dashboard/API once saved (only the last 4 characters shown) — only a
present/absent flag or masked value is ever exposed.

**Prefer setting API keys via environment variables.** Values from env
vars are only ever held in memory — they are never written to disk. A key
entered in the dashboard, by contrast, is stored in plain text in
`config.json` (file mode `0600`). You can mix the two freely, e.g. set
`REAPARR_SETTING_SEERR_URL` via env and enter only the Seerr API key in the
dashboard.

## Dashboard

Reaparr serves a small web dashboard on **port 8767** with three tabs:

- **Due for deletion** — every watched movie and fully watched TV season,
  with when the scheduled run will delete it ("Will be deleted in"). It
  uses the exact same fetch and matching code the scheduled sweep uses
  (not a separate, potentially-diverging check). The list is held in
  memory only and rebuilt on startup, every 15 minutes, after any dashboard
  save, and when you press Refresh.
  **The grace period only governs the scheduled run.** Anything in the list
  can be deleted by hand right away: **Delete now** for one item, or tick
  rows (or the header checkbox for all) and **Delete N selected** (both
  after a confirmation). A manual delete re-checks the item live first
  (still watched; a season still fully watched), then calls the same
  Radarr/Sonarr delete path the scheduled run uses; deleting a selection
  also runs the Seerr cleanup.
- **Settings** — grace periods, poll schedule, log level, and the daemon
  toggle, editable unless locked by an env var (see "Configuration" above).
  With the daemon disabled nothing is deleted automatically — the header
  shows "Manual only" — and deleting happens only via the buttons above.
- **Connections** — Jellyfin/Radarr/Sonarr/Seerr URL + API key, each with a
  "Test connection" button, editable unless locked by an env var.

A settings/connections change made in the dashboard takes effect on the
very next sweep — no restart required.

### Login

The dashboard is protected by a login, provided by
[authy](https://github.com/Miista/authy). It is **on by default**, with no
configuration at all: the first time you open the dashboard you create a local
account (username and password) on `/auth/setup`, and every later visit signs
in on `/auth/local`. A **Sign out** card appears at the bottom of
Settings → General.

The account (a bcrypt hash in `local.json`, never the password) and the key
that signs sessions (`auth.json`) live in `/app/data/auth/`, in the same volume
as `config.json`. The directory must be writable by the container's user.
**Whoever opens the dashboard first creates the account**, so open it yourself
before exposing it. To reset a forgotten password, stop the container, delete
both `/app/data/auth/local.json` and `/app/data/auth/auth.json`, and start it
again; the setup page returns. (Deleting only `local.json` would leave sessions
from the old account valid until they expire.)

**Using forward auth instead?** Set `REAPARR_AUTH_DISABLED=true` to turn the
login off. The dashboard is then unprotected by reaparr itself, so keep it
behind your proxy's auth.

#### Roles (optional, OIDC only)

With OIDC, who may do what can follow groups at the identity provider. Three
variables name a group each. A person holds every role whose group they are
in, and each action lists the roles it accepts, any one of which is enough.
Since every list includes the roles above it, an admin may do everything and
a curator everything a user may:

| Action | Roles that may | Role comes from |
|---|---|---|
| see what is due and the library, keep and unkeep | user, curator, admin | `REAPARR_ROLE_USER_GROUP` → user |
| delete due titles by hand | curator, admin | `REAPARR_ROLE_CURATOR_GROUP` → curator |
| change settings and connections | admin | `REAPARR_ROLE_ADMIN_GROUP` → admin |

With none of the three set, everyone the provider lets in holds every role,
as before roles existed. With any set, someone in none of the groups is
signed in but sees only a "no access" page. `REAPARR_ROLE_USER_GROUP` may be
left empty to make anyone the provider lets in a user. The local account,
when enabled, holds every role. The scheduled sweep is not affected by roles.

Every route checks the roles on the server, so the page hiding a button is a
courtesy, not the control. A person's groups are read when they sign in and a
session lasts 12 hours without use, so a change at the provider can take up to
that long to reach reaparr.

#### OpenID Connect (optional)

To sign in through an identity provider (Authelia, Authentik, Keycloak,
Entra ID, …) instead, set all four of these. OIDC is on as soon as they are
there; there is no separate switch:

| Variable | |
|---|---|
| `REAPARR_OIDC_ISSUER` | provider URL, e.g. `https://auth.example.com` |
| `REAPARR_OIDC_CLIENT_ID` / `REAPARR_OIDC_CLIENT_SECRET` | the client registered at the provider |
| `REAPARR_OIDC_BASE_URL` | reaparr's external URL, e.g. `https://reaparr.example.com` |
| `REAPARR_LOCAL_ENABLED` | optional; `true` keeps the local account next to OIDC as an escape hatch (see below) |

Register `<BASE_URL>/auth/callback` as the client's redirect URI (the exact
value is logged at startup). Who may sign in is decided at the provider.

With OIDC configured, a page load goes straight to the provider and the local
account is switched off: **setup is closed**, so nobody can claim the dashboard
by being first. If the provider doesn't approve a login, or can't be reached,
you get a page with a retry button, never a redirect loop. With
`REAPARR_LOCAL_ENABLED=true` that page also offers the local account, the way
in when the provider is down; setup is then open until the account exists, so
create it right away. Only some of the OIDC settings, or none with
`REAPARR_LOCAL_ENABLED=false`, makes reaparr refuse to start; an unreachable
provider does not (it retries in the background). See the authy README for the
full flow.

## Deployment

Reaparr is a single static binary that both runs the scheduled sweep and
serves the dashboard on port 8767. It persists dashboard-entered
configuration in `/app/data/config.json` (fixed path) — mount a
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
    REAPARR_SETTING_JELLYFIN_URL: http://jellyfin:8096
    REAPARR_SETTING_JELLYFIN_API_KEY: ${JELLYFIN_API_KEY}
    REAPARR_SETTING_RADARR_URL: http://radarr:7878
    REAPARR_SETTING_RADARR_API_KEY: ${RADARR_API_KEY}
    REAPARR_SETTING_SONARR_URL: http://sonarr:8989
    REAPARR_SETTING_SONARR_API_KEY: ${SONARR_API_KEY}
    REAPARR_SETTING_SEERR_URL: http://seerr:5055
    REAPARR_SETTING_SEERR_API_KEY: ${SEERR_API_KEY}
    REAPARR_SETTING_MOVIES_GRACE_PERIOD: 2d
    REAPARR_SETTING_TV_GRACE_PERIOD: 7d
    REAPARR_SETTING_POLL_SCHEDULE: "@hourly"
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
```

The image just copies in a pre-built binary, so cross-compile it with Go
first (use `arm64` instead of `amd64` for an ARM host):

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/linux/amd64/reaparr .
docker build --platform linux/amd64 -t reaparr .
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
