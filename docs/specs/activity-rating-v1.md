# Activity percentages: legacy formula versions

`GET /api/v1/activity/view/activity` delegates to the Activity owner through
`ViewActivityRatings`. Core only authenticates the viewer and validates the HTTP
shape. Activity verifies the panel/version/installation policy, scopes employees,
and reads stored observations through CRM Events. There are no new amoCRM reads.

The request explicitly supplies the selected account day, event-loading `from/to`,
clock window in minutes, surface, and optional Unix cutoff. Dashboard requests the
same work-window event range used by its existing UI and passes the loaded events'
`snapshotAt` cutoff on today's view. Profile requests its full calendar day. The
account timezone comes from the panel, never a client-controlled timezone string.
Changing the personal display window requests a new calculation.

The result echoes this context and includes `formula_version`, `snapshot_at`,
`partial`, and employee percentages. Dashboard batches panel employees. Profile
requires one employee and returns `minute_percentages[1441]`: percentages ready for
each local clock minute 0..1440. The UI selects a number as its clock advances; it
does not divide counts, classify events, or calculate percentages. Both profile
consumers (avatar/theme and general information, including the previous day) use
that same result. The TypeScript formulas remain only as fixture/demo references.

## Preserved behavior

- `dashboard-v1`: all observed event types, two-minute bins, an ordinary event
  covers one minute, call types containing `call` use the call duration rounded
  up to minutes if available. Each active bin contributes two minutes, including
  a final shorter bin. Percent is active minutes / elapsed minutes, capped at100
  and rounded to one decimal. The existing hour-based current-day cutoff is kept,
  including its treatment of an odd configured end minute.
- `profile-v1`: the frozen legacy substring/custom-field whitelist; an ordinary
  event marks its starting bin. Only exact incoming/outgoing calls use duration,
  with the legacy60second default for zero or unknown duration. Only bins whose
  end has passed the local clock cutoff are counted. A historical day uses the
  whole window. The event timestamp is reduced to its account clock minute, so
  seconds do not introduce an accidental formula change.
- Dashboard applies the saved pipeline filter (events without a pipeline stay
  visible); profile deliberately retains its prior unfiltered behavior.
- The existing ascending60x100 event-page limit is retained and exposed as
  `partial=true` when more observations remain. Repeated cursors fail explicitly.
  Legacy call-card fallback is bounded to150 missing facts, before the pipeline
  filter, as in the previous viewer adapter. Current projected `CallFacts`,
  including explicit unknown and known-zero values, take precedence.

The rating read and the already-loaded event view are independent live reads,
just like other existing panel resources. This is not an immutable cross-request
snapshot guarantee. The explicit cutoff is preserved; new observations or refreshed
call facts between reads may change either response normally.

`docs/fixtures/activity-rating-v1.json` and the identical web-panel test fixture
freeze25 representative legacy cases: duplicates, unknown types, call durations,
odd windows/cutoffs, seconds, fractional timezones and DST. Both Go owner formulas
and unchanged TypeScript references must reproduce these numbers. HTTP, mTLS,
pagination, scope, call projection and fallback tests cover the production path.

## Bounded rating work and deadlines

The rating-only HTTP and exact `Activity/ViewActivityRatings` RPC use a25second
budget, below the30second delegation lifetime. All other RPC methods retain their
10second budgets, including each underlying CRM Events RPC read. Embedded Activity
also applies the rating budget; cancellation propagates to page and card reads.
The rating response alone receives a26second write deadline (25seconds of work
plus one second to send the result); the shared HTTP server retains its15second
WriteTimeout for all other routes.

The60page scan retains only compact observations. Once it finishes, at most150
legacy missing call cards are resolved by four workers (the same concurrency as
the legacy UI). Pipeline filtering is applied afterward. Resource exhaustion stops
new card work; a cancelled request waits for the bounded in-flight workers to exit.
No raw event payload cache or unbounded goroutine pool is introduced.

A controlled latency audit used60query pages at50ms per page and150card reads
at200ms each. The previous serial path hit its10second deadline after one page
and49card attempts; the bounded four-worker path completed all60pages and150cards
in10.72seconds under the rating-only25second budget. This is a deterministic fake
source measurement, not a production latency guarantee. Slower reads still fail
at the finite deadline rather than return an incomplete score as complete.

The rounding oracle in `docs/fixtures/activity-rating-rounding-v1.json` checks
1,041,840 count pairs against JavaScript, including the 72 cases that differ
if the original multiplication order is reassociated. Numeric pipeline IDs are
normalized as decimal strings, including ordinary seven- and nine-digit CRM IDs.
