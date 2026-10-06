# Contributing

Thanks for looking. agentsafe's value is that its guarantees are proven, not claimed, so the bar for a change is what the change can prove.

**Security issues:** please don't open a public issue; see [SECURITY.md](SECURITY.md).

## Setup

```bash
git clone https://github.com/Ashutosh2308Bhardwaj/agentsafe && cd agentsafe
scripts/check.sh   # every gate CI runs, stopping at the first failure
```

The core module needs Go 1.23+. The `sqlite`, `postgres`, `anthropic` and `gemini` modules each have their own `go.mod` and minimum Go (their dependencies'); the go command fetches the toolchain they need. `scripts/check.sh` also needs [golangci-lint](https://golangci-lint.run) and [staticcheck](https://staticcheck.dev) on your `PATH`. For the Postgres tests, point `AGENTSAFE_POSTGRES_URL` at a database they may write to; without it they skip locally (CI runs them against Postgres 14 and 17).

Run `scripts/check.sh` before every commit, and read its exit code, not just its last lines: `bash scripts/check.sh > check.log 2>&1; echo $?`.

## What a change needs

1. **A test that fails without it.** Before sending a fix or a feature, break the code you wrote on purpose (delete the check, invert the condition) and confirm a test goes red. A test that passes either way proves nothing. Make sure the broken version *compiles*: a build failure isn't a caught bug.
2. **No new dependencies in the core module.** It is standard library only, on purpose. Anything that needs a dependency (a storage backend, a model SDK) is its own module, like `sqlite/` or `anthropic/`.
3. **Errors a caller can act on are sentinels** (`errors.Is`), in `errors.go` or next to the feature they belong to.
4. **Every I/O call takes a `context.Context`**; pure functions don't.
5. **Comments say why**, especially when the code chose the less obvious option. Many of agentsafe's choices exist because of a failure in [docs/FAILURES.md](docs/FAILURES.md); name it.

## Changing the log format

The log outlives the code that wrote it. Follow [FORMAT.md](FORMAT.md): only add fields; bump `FormatVersion` unless the field's absence is harmless; add a golden log to `testdata/`. **Never edit or delete a golden log**: they prove that every log ever written still rebuilds. The same care applies to idempotency keys (`Canonical`): a change that alters an existing key can make a resumed run act twice.

## Storage backends

A new backend implements `LineStore` (and ideally `Locker`) and must pass `storetest.Run`. Its conditional append ("line N only if N-1 lines exist") must be atomic against every writer in every process; that is the fencing the runner's safety rests on.

## Commits and pull requests

One logical change per commit, with a message that says what changed and why. Pull requests run the full CI matrix (Linux, macOS, Windows; several Go versions; Postgres; fuzzing; the crash harness); a red CI isn't merged. By contributing you agree your contribution is licensed under the [Apache License 2.0](LICENSE).
