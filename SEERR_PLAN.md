# Seerr stale-request cleanup — design (not yet implemented)

## Why

When Reaparr deletes a title via Radarr/Sonarr, Seerr's own "Media
Availability Sync" job (runs daily by default) correctly updates that
title's `media.status` to `DELETED` (verified: status enum value `7`,
confirmed directly from Seerr's source at
`server/constants/media.ts`) — but it does **not** touch the associated
`request` record(s), which stay stuck at whatever status they were at
before (typically `5` = `AVAILABLE`). This is a known, real gap — see
`seerr-team/seerr` issue "Media remains in 'requested' status after being
deleted" — not something Reaparr can influence from outside.

Practical impact confirmed live against this household's real Seerr
instance: two titles genuinely show `media.status: 7` with their nested
`request.status` still `5`. This does NOT block re-requesting the title
(Seerr's frontend correctly keys off `media.status`, not the stale
`request.status`) — but it is genuine orphaned data, growing over time,
that nothing currently cleans up.

## Decided

- **Reaparr will actively clean these up**, as a new, independent sweep
  step — not a side-effect of the Radarr/Sonarr delete call itself.
- **Query-based, not delete-triggered**: each sweep independently asks
  Seerr "which requests currently have deleted media?" and cleans up
  whatever it finds — rather than trying to track "did I just delete this
  title, so I should also clean up its Seerr request" as a one-shot
  action. This is deliberately more robust:
  - Self-healing: catches titles deleted by anything (Reaparr, manual
    cleanup, anything else), not just Reaparr's own actions this sweep.
  - No retry bookkeeping needed: if a Seerr delete call fails, the
    request/media record is still there next sweep (still `status: 7`),
    so it naturally gets retried for free — no state needs to be
    persisted to remember "this one failed, try again."
  - Fully decoupled from the Radarr/Sonarr deletion loop — simpler to
    test and reason about in isolation.
- **Deletion target: the `Media` record, not individual `MediaRequest`
  rows.** Confirmed from Seerr's actual TypeORM entity source
  (`server/entity/Media.ts`):
  ```ts
  @OneToMany(() => MediaRequest, (request) => request.media, {
    cascade: ['insert', 'remove'],
  })
  public requests: MediaRequest[];
  ```
  `cascade: ['insert', 'remove']` means Seerr's own backend automatically
  removes every associated `MediaRequest` when the `Media` row is removed
  via the ORM — confirmed this is exactly what Maintainerr's own
  `deleteMediaItem`/`removeMediaByTmdbId` methods rely on (see prior
  research in this conversation). One `DELETE /media/{id}` call is
  sufficient; Reaparr does not need to separately enumerate and delete
  each request.
  - Why deleting the whole media record (not just one request) is
    correct, not just simpler: once the underlying file is genuinely
    gone, no request row pointing at that title means anything anymore —
    whether it's a stale active request or older declined-then-abandoned
    history. There's no case where you'd want to keep a request for
    media that no longer exists.
- **Optional feature, gated by config**: `SEERR_URL` / `SEERR_API_KEY`.
  If unset, zero Seerr awareness, zero behavior change from today.
- **Permissions**: confirmed via Seerr's actual `hasPermission()` source
  that a user with only the `ADMIN` bit set (value `2`) gets an
  unconditional bypass on every permission check, including
  `MANAGE_REQUESTS` (value `16`) — so the household's existing Seerr API
  key (currently `permissions: 2`, admin-only) already has sufficient
  access; no separate permission grant is needed.
- **Failure handling**: log a warning per failed delete, don't retry
  explicitly (see "self-healing" above — it retries implicitly next
  sweep), and don't count Seerr cleanup failures against the sweep's
  main `cleaned`/`failed` counters, since Seerr tidiness is secondary to
  the actual Radarr/Sonarr deletion which already succeeded independently.
- **Stateless**, matching the rest of Reaparr — no new persisted state.

## Confirmed API surface (tested live against this household's Seerr
instance, not guessed from docs)

- `GET /api/v1/request?take=N&skip=N&filter=deleted` — **server-side
  filtered**, confirmed working correctly (returned exactly the 2 known
  stale requests, out of 13 total). This is the right endpoint —
  filtering client-side against `/api/v1/media` was also tested and
  works, but `/request?filter=deleted` is simpler since it directly
  returns the nested `media.id` needed for the delete call, in one
  request.
  - NOTE: `GET /api/v1/media?filter=deleted` does NOT work — the
    `filter` param is silently ignored on that endpoint (confirmed: a
    test request returned unfiltered results starting with
    `status: 1`). Use `/request`, not `/media`, for the filtered query.
  - Pagination: response has `pageInfo: { pages, pageSize, results,
    page }` — same shape as Jellyfin's activity log, paginate with
    `skip`/`take` until `skip >= pageInfo.results`.
- `DELETE /api/v1/media/{id}` — deletes the media record and cascades to
  its requests (see above). `{id}` is Seerr's own internal media ID
  (e.g. `21`, `11` in the examples above) — NOT the TMDB/TVDB ID. Get it
  from `request.media.id` in the `/request?filter=deleted` response.
- Auth: `X-Api-Key` header, same pattern as the existing Radarr/Sonarr/
  Jellyfin clients in this codebase.

## Implementation plan

1. **`config.go`**: add `SeerrURL string`, `SeerrAPIKey string` — optional,
   following the exact `RADARR_URL`/`RADARR_API_KEY` pattern (env-configurable
   URL with a sane default like `http://seerr:5055`, API key required only
   if URL/feature is meant to be used — no validation error if both are
   empty, since this is fully optional).
2. **New file `seerr.go`**: `seerrClient` struct (`baseURL`, `apiKey`,
   `httpClient`, `log zerolog.Logger` — same shape as `arrClient`/
   `jellyfinClient`). Methods:
   - `hasSeerr() bool` — same pattern as `arrClient.hasRadarr()`.
   - `deletedRequests() ([]seerrDeletedMedia, error)` — paginates
     `GET /request?filter=deleted`, returns a flat list of `{mediaID int,
     title string}` (title for logging; pull from `request.media` — note
     the `/request` response's nested `media` object does NOT include a
     `title` field based on what was fetched live — may need a
     supplementary `GET /movie/{tmdbId}` or `GET /tv/{tmdbId}` call per
     item to get a human-readable title for the log line, OR just log by
     tmdbId/mediaId if that turns out to be simpler; verify this when
     implementing rather than assume).
   - `deleteMedia(mediaID int) error` — `DELETE /media/{id}`, same
     error-handling shape as `arrClient.doDelete` (log the attempt, log
     success/failure, return error to caller).
3. **`sweep.go`**: new method `cleanUpSeerr()` called once per
   `sweepOnce()`, independent of (and not blocking) the existing
   Radarr/Sonarr deletion loop. Structure:
   ```go
   if s.seerr.hasSeerr() {
       deleted, err := s.seerr.deletedRequests()
       if err != nil {
           log warning, return early (don't fail the whole sweep)
       }
       for each item: s.seerr.deleteMedia(item.mediaID), log per-item
       result, tally a count for the sweep-finished summary line
   }
   ```
   Add a `seerr_cleaned`-style count to the existing
   `"sweep finished: %d due, %d deleted, %d skipped, %d failed"` summary
   line, or a separate log line — decide based on what reads cleanest
   when actually implementing (don't over-think this at plan time).
4. **`main.go`**: wire up `seerrClient` construction alongside
   `jellyfin`/`arr`, pass into `sweeper`.
5. **Tests**: mirror the existing test patterns in `sweep_test.go`/
   `arr_test.go` — a fake Seerr HTTP server (httptest), test cases for:
   - Seerr not configured → no calls made, no error.
   - Deleted requests found → each gets a DELETE call.
   - A DELETE call fails → logged, doesn't fail the sweep, doesn't block
     other items in the same batch.
   - Pagination works across multiple pages (mirror
     `TestSweepOnce_ActivityLogPagination`'s pattern).
6. **README.md**: add `SEERR_URL`/`SEERR_API_KEY` to the Configuration
   table, and a short paragraph under "How it works" or a new section
   explaining this cleanup step and why it exists (the Availability Sync
   gap).
7. As always: `gofmt -l .`, `go vet ./...`, `go test ./... -race -count=1`
   before committing. Commit, push, confirm CI (test + publish) goes
   green, then redeploy on the OptiPlex (`docker compose pull reaparr &&
   docker compose up -d reaparr` from `~/docker/optiplex`, NOT from the
   `media/` subdirectory — see git history for why that distinction
   matters) with `SEERR_URL`/`SEERR_API_KEY` added to the compose env.

## Open question for whoever picks this up

The exact shape of `seerrDeletedMedia` / whether a title lookup is needed
for a good log line wasn't fully resolved — the live `/request?filter=deleted`
response was fetched and inspected for `id`/`status`/`media.status`/
`media.tmdbId`, but not checked closely for a `media.title`-equivalent
field. Check the real response shape before assuming a second API call
is needed just to get a readable title for logging.
