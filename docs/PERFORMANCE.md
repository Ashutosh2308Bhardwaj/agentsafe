# What the safety costs

agentsafe does more per step than a plain agent loop: every consequential transition is logged before it takes effect, hash-chained, optionally sealed, and written durably. This page measures that cost, so "all this safety must be slow" can be answered with numbers.

**Summary: about 16 µs of CPU per tool call, plus three durable writes.** The CPU part is negligible next to a model call (typically 0.5–5 s). The durable writes are the real cost, and they are a property of your disk, not of agentsafe: measure them on your hardware with the commands below.

## Results

Apple M5 (10 cores), macOS, Go 1.24, internal SSD. Median of 3 runs.

### Per step

| Benchmark | Time | Allocations | What it is |
|---|---|---|---|
| `BenchmarkRunnerStep` | **15.6 µs per tool call** | ~40 KB, 178 allocs | The whole loop for one idempotent call: model decision (scripted, free), key derivation, three appends (`model_decided`, `tool_started`, `tool_result`), the state transitions. In-memory store: CPU only. |
| `BenchmarkAppend/memory` | 0.9 µs | 1.5 KB, 6 | One event: encode, hash-chain link. |
| `BenchmarkAppend/memory+hmac` | 1.1 µs | 2.0 KB, 12 | With an HMAC chain (`FileLog.Key`). |
| `BenchmarkAppend/memory+sealed` | 2.0 µs | 3.9 KB, 21 | With sealing (AES-256-GCM). |
| `BenchmarkAppend/file+fsync` | **3.8 ms** | 1.9 KB, 15 | One event to `FileLog`: the same, plus a durable write (`F_FULLFSYNC` on macOS). |
| `BenchmarkAppend` (sqlite) | **4.0 ms** | 1.5 KB, 27 | One event to the SQLite backend (WAL, `synchronous=FULL`, `fullfsync`). |
| `BenchmarkCanonical` | 3.4 µs | 4.3 KB, 104 | Canonical form of a nested argument object (the input to a key). |

A tool call costs three appends, so with `FileLog` on this machine: 3 × 3.8 ms + 16 µs ≈ **11.5 ms per tool call**, almost all of it waiting for the disk.

### Resuming a run

What a process pays to pick up a run: read the log, verify the hash chain (and open sealed events), rebuild the state.

| Events in the run | Read + verify + rebuild | Sealed | Rebuild alone |
|---|---|---|---|
| 102 | 0.28 ms | 0.43 ms | 0.012 ms |
| 1,002 | 2.7 ms | 4.2 ms | 0.12 ms |
| 10,002 | 27.5 ms | 42.5 ms | 1.3 ms |

Linear in the length of the run, and dominated by decoding and verifying lines, not by the state machine. A run of a few hundred tool calls resumes in a few milliseconds.

### Reconciliation

| Effects | `reconcile.Audit` |
|---|---|
| 100 | 0.11 ms |
| 1,000 | 1.3 ms |
| 10,000 | 11.7 ms |

Every check in full: each expected effect matched field by field (`Same`, exact decimals), each traced to a logged result.

## What the numbers don't show

- **Durable writes depend on the hardware and OS.** On macOS a durable write must use `F_FULLFSYNC` (a plain `fsync` leaves data in the drive's cache, where a power cut loses it), and that takes milliseconds. Linux `fsync` on server NVMe with power-loss protection is typically much faster. These numbers are from a laptop; run the benchmarks where you deploy.
- **Postgres** isn't measured here: its cost is a network round trip plus the server's commit (`synchronous_commit`), so it depends on your database, not on agentsafe.
- **The model** dominates real runs. The scripted model in `BenchmarkRunnerStep` answers instantly.

## A finding: SQLite on macOS wasn't durable

The first SQLite measurement was 75 µs per append, 50× faster than the file store. That wasn't speed: SQLite's `synchronous=FULL` issues a plain `fsync`, which on macOS returns once the drive's cache has the data, so a power cut could lose a committed event. `sqlite.Open` now also sets `fullfsync` (ignored on other platforms), and the append costs what a durable write costs. `TestOpenSetsTheDurabilitySettings` reads every durability pragma back from a connection.

## Reproduce

```bash
go test -run '^$' -bench . -count 3 . ./reconcile/      # core and reconcile
(cd sqlite && go test -run '^$' -bench . -count 3 .)     # SQLite backend
```

The benchmarks are in `bench_test.go`, `reconcile/bench_test.go` and `sqlite/bench_test.go`.
