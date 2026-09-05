# Fair-claim scheduler: implementation and performance evidence

Date: 2026-09-05. Baseline: `386ad68`, schema 000009.

## Implemented changes

- The live-lease predicate separates platform jobs from integration jobs and
  stops counting at the cap. It continues to evaluate expiry at database
  statement time; there are no cached counters to reconcile.
- Migration 000010 adds `jobs_exhausted_idx` and `jobs_platform_ready_idx`.
- Workers wake after local job finalization releases a slot. A bounded refill
  can issue successive full batches if BatchSize is smaller than free capacity.
  Partial/empty results and errors stop that refill; the timer remains a fallback.
- The global transaction advisory lock, sequential per-slot READ COMMITTED
  statements, integration-wide cap, priority ordering, lease fencing and atomic
  reaping/observers remain in place.

## Method

PostgreSQL 17.10, aarch64, Docker; Go 1.25 tooling in Docker. PostgreSQL settings:
shared_buffers 128 MB, work_mem 4 MB, effective_cache_size 4 GB, random_page_cost 4,
JIT on. No remote database RTT or production traffic was simulated.

`TestFairClaimPerformance` runs eight deterministic scenarios with one and eight
claimers. Each path gets two excluded warmup waves and twelve measured waves.
A wave starts its claimers together and each requests ten jobs. Between waves,
claimed rows and reaping fixtures are restored and VACUUM ANALYZE runs outside
timing. Cap is 64 unless specified below, so full batches remain available even
with eight claimers; it is a benchmark parameter, not a recommended runtime cap.

The frozen baseline SQL and production SQL are both run on each installed schema.
The table compares the original SQL on schema 000009 against production SQL on
schema 000010, including both added indexes. All runs retain exact seeds, query
hashes, schema/index definitions, initial-seed plans and raw samples under
`tmp/fair-claim-performance/`. Directory `schema9` contains the first run;
`schema10` contains the first complete final-schema run. These local artifacts
are intentionally outside version control; the harness reproduces new samples.

Wall times include pool acquisition, BEGIN, lock wait, reaping, slot queries and
COMMIT. The lock-query and lock-hold fields are client-observed intervals with
network overhead, not pure server wait/hold times. The first-seed EXPLAIN plan
does not represent every subsequent slot: additional live leases and cached
plans can change execution. Timing runs did not use the race detector.

## First complete comparison

Wall p50 / p95, milliseconds per claim transaction:

| Scenario | Claimers | Before | After |
| --- | ---: | ---: | ---: |
| 100k ready jobs, 2 installations | 1 | 9.50 / 10.93 | 3.24 / 3.69 |
| Same | 8 | 47.16 / 87.20 | 15.53 / 26.97 |
| 2 integrations, cap 2, 10k ready | 1 | 2.74 / 3.05 | 1.89 / 2.17 |
| Same | 8 | 8.79 / 15.22 | 5.40 / 9.03 |
| 1000 lanes, only 2 active, 10k ready | 1 | 41.91 / 53.69 | 28.95 / 29.56 |
| Same | 8 | 277.72 / 664.13 | 171.23 / 496.79 |
| 2 integrations, 300 installations each, 12k ready | 1 | 14.74 / 15.21 | 10.31 / 13.91 |
| Same | 8 | 78.05 / 164.53 | 42.34 / 84.32 |
| 100 lanes, 90 at cap 10, 900 live leases | 1 | 75.67 / 81.61 | 6.03 / 6.68 |
| Same | 8 | 414.62 / 762.68 | 28.60 / 47.38 |
| 100k future high-priority jobs + 10k ready | 1 | 34.05 / 66.43 | 16.88 / 18.01 |
| Same | 8 | 144.55 / 259.85 | 148.00 / 571.25 |
| 100k integration jobs + 1000 platform jobs | 1 | 21.81 / 27.94 | 4.11 / 4.75 |
| Same | 8 | 99.52 / 182.15 | 18.59 / 33.44 |
| 10k ready + 20 expired + 20 exhausted, transactional observer | 1 | 20.35 / 21.82 | 18.23 / 20.15 |
| Same | 8 | 56.04 / 94.53 | 75.61 / 128.32 |

Every full-batch row represents 120 returned jobs / 12 transactions with one
claimer, or 960 / 96 with eight. The cap-2 scenario returns only four jobs per
wave: 48 total with either concurrency, and seven of eight claimers can receive
nothing. Reaping runs verify forty observer writes per wave and the persisted
audit rows. Returned IDs are checked for duplicates and live counts for cap
violations. These are **admission measurements**, not handler executions or a
drain of 100,000 jobs.

The future-job and nonempty-reaper eight-claimer rows did not improve in the
first final-schema run. They must be retained alongside the improvements and
examined separately; this table is not evidence of a universal speedup. A second
non-paired run in `schema10-repeat` also showed substantial drift, including in
unchanged reaping and COMMIT statements. The paired comparison below provides
the stronger evidence for the SQL change.

## Paired confirmation on schema 000010

The final harness restores the same seed between paths and alternates A/B then
B/A within every wave. Twelve paired waves follow two paired warmups. Artifacts
are in `schema10-paired`; runtime metadata records Go 1.25.12, arm64, GOMAXPROCS 2.
Both paths have the two new indexes, so this comparison isolates the SQL rewrite
and does not measure the index benefit a second time.

Wall p50 / p95, milliseconds per claim transaction:

| Scenario | Claimers | Original SQL | Updated SQL |
| --- | ---: | ---: | ---: |
| 100k ready | 1 | 3.53 / 4.65 | 3.52 / 4.44 |
| Same | 8 | 15.92 / 27.81 | 14.59 / 28.68 |
| Two integrations, cap 2 | 1 | 2.03 / 2.55 | 2.14 / 2.36 |
| Same | 8 | 5.40 / 8.53 | 5.89 / 9.35 |
| 1000 lanes, two active | 1 | 33.19 / 36.23 | 29.62 / 33.47 |
| Same | 8 | 227.31 / 609.76 | 191.34 / 537.60 |
| 600 installations | 1 | 13.37 / 13.95 | 10.31 / 10.91 |
| Same | 8 | 63.25 / 115.04 | 51.56 / 90.87 |
| 90 of 100 lanes at cap | 1 | 71.09 / 75.84 | 6.37 / 6.76 |
| Same | 8 | 328.25 / 649.75 | 33.88 / 58.64 |
| 100k future-priority jobs | 1 | 20.54 / 25.17 | 16.91 / 20.59 |
| Same | 8 | 80.99 / 145.95 | 67.07 / 117.60 |
| 100k integration + 1000 platform jobs | 1 | 6.22 / 7.16 | 3.32 / 3.69 |
| Same | 8 | 26.30 / 46.50 | 14.30 / 27.30 |
| Nonempty reaping with observer | 1 | 12.69 / 14.01 | 10.03 / 11.33 |
| Same | 8 | 30.63 / 49.89 | 20.96 / 30.97 |

For future jobs with eight claimers, median summed slot-query time changed
16.65 to 13.42 ms, while reaper time stayed 0.47/0.46 ms. In the observer case,
slot-query time changed 4.99 to 2.37 ms, with comparable empty-reaper times for
the claimers following the first reaper. The earlier large regressions did not
reproduce under paired ordering. Their exact cause was not proven; the initial
runs are retained rather than discarded.

The cap-2 microcase does show a small cost for the rewritten predicate: roughly
0.11 ms per single-claimer transaction and 0.49 ms median with eight claimers
in this run. On a 100k ready backlog with only two installations, most of the
total improvement comes from the exhausted index, not the count rewrite.

## Isolated findings and rejected changes

- Before the index, an empty exhausted-job scan with 100,004 unrelated rows took
  6.162 ms and 1429 shared-buffer hits. With the partial index, the same
  transactional experiment took 0.027 ms and one hit. This is one query sample,
  not a throughput forecast; its plan is in `reap-index-experiment.txt`.
- In the saturated-lane seed, the old active probe repeatedly scanned roughly
  900 processing rows. The rewritten predicate used installation probes, with
  selected-query buffers 2499 to 480 and initial-plan execution 6.35 to 0.76 ms.
  On schema 000009 alone, the ten-slot batch p50 changed 75.67 to 6.98 ms.
- The existing installation-ready index does support NULL installations, but
  the observed platform plan bitmap-scanned and sorted all 1000 platform rows.
  The platform-specific index changed this branch to an ordered one-row scan
  around 0.007 ms, instead of roughly 0.5 ms of candidate selection.
- An experimental `(last_claimed_at, scope_id)` lane index allowed early stopping
  in the first-slot plan and reduced candidate locks for 600 installations.
  It did not improve full batches in the four-wave experiment: the idle-lane
  p50 was 58.11 ms and saturated-lane p50 11.93 ms. It was dropped and is not
  included in the migration. Metadata/plans are in `rotation-index`.

The indexing experiments include their own metadata; the rotation experiment
already had the exhausted index. Do not attribute whole-transaction changes
from that experiment to the lane index alone.

## Actual worker execution

`TestWorkerPollingPerformance` completed all 24 jobs in each run, with three runs
per mode and alternating order. Both modes use the same production SQL and
finalization. Configuration: two integrations, cap 2, concurrency 4, batch 10,
poll 1 second, handler 50 ms. Completion timestamps are observed after COMMIT;
SQL independently verifies all 24 completed jobs and 24 completed attempts.

| Mode | Median wall time | Range, three runs |
| --- | ---: | ---: |
| Original ticker-only loop | 5.068 s | 5.066–5.080 s |
| Local completion wakeup | 0.341 s | 0.334–0.344 s |

This is actual execution of 24 synthetic jobs per run, including durable
completion. It is not an amoCRM request-rate estimate or a 100k-job benchmark.

## Limits and next decisions

Empty lanes and installations are still traversed on each slot; many future
high-priority rows can still make priority-first lookup expensive. Reaping and
observer writes still extend the global critical section. Remote RTT and a
sustained realistic worker workload remain separate measurements.

Per-slot pgx batching was deferred: it would submit queries even after capacity
is exhausted, requires collecting every returned job and needs explicit handling
of protocol/time semantics. The local lock-query baseline is around 0.05 ms;
the supported wins here come from SQL work and worker wakeups. No active/ready
counters, asynchronous reaper or per-lane locking were introduced without a
complete design for expiry, delayed readiness and atomic observers.

Reproduction commands and artifact definitions are in the
[capacity runbook](../runbooks/widget-capacity.md#scheduler-performance-and-reproduction).

## Validation

- `make test`: Docker formatting check, full `go vet ./...`, all package tests
  with the race detector passed.
- `make integration-test TEST_COMPOSE_PROJECT=amocrm-fair-regression-test`:
  PostgreSQL integration suite with the race detector passed; this also checked
  migration checksums, guarded down, complete down/up and concurrent migration up.
- The final paired admission matrix passed all eight scenarios with one/eight
  claimers and verified returned counts, cap, IDs and observer persistence.
- Worker scheduling tests cover completion wakeup for fair/legacy paths,
  filling multiple batches, partial/capped/empty backoff, claim timeout and
  cancellation/drain. Selection tests cover priority across installations,
  SKIP LOCKED, future readiness and expired leases beyond the reap bound.

Only disposable test databases were migrated. Applying 000010 to a running
installation uses transactional index creation and can make job-table writes
wait during index construction; the runbook describes that deployment constraint.
