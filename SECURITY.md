# Security policy

agentsafe exists to stop an AI agent from moving money twice, without approval, or without a trace. A way to defeat any of that is a security vulnerability, even if no memory is corrupted and no secret leaks.

## Reporting a vulnerability

**Do not open a public issue.** Report privately through GitHub: [Report a vulnerability](https://github.com/Ashutosh2308Bhardwaj/agentsafe/security/advisories/new) (the repository's *Security* tab).

Please include what you did, what you expected agentsafe to do, what it did instead, and a way to reproduce it (a test, a log file, a sequence of calls). A failing Go test is the most useful report there is.

This is maintained by one person. You can expect an acknowledgement within 7 days and an assessment within 14. Fixes for confirmed issues are released as soon as they're ready, with a GitHub security advisory crediting you (unless you prefer otherwise). Please give us a reasonable chance to release a fix before disclosing publicly; 90 days is the default.

## What counts

Anything that breaks a guarantee in [README.md](README.md#whats-proven), for example:

- **Duplicate effects:** a way to make an idempotent tool act twice for one operation (crash, retry, timeout, concurrent runners, a resumed run, a crafted model response).
- **Approval bypass:** a gated call that runs without an approval, with a rejected approval, or approved by someone the `Authorizer` should refuse.
- **History forgery:** a change to a log that `Read` accepts without `ErrTampered` (with a key, or anywhere but the last line without one: see [FORMAT.md](FORMAT.md) and `chain.go` for the documented limits).
- **Sealed data exposure:** content of a sealed log readable without its key, or a sealed payload that opens on the wrong line.
- **Wrong reconciliation:** `reconcile.Audit` passing a run where money moved without authorization, moved twice, or doesn't match what should exist.
- **Resource exhaustion from model input:** arguments or responses a model can send that make agentsafe hang, crash, or allocate without bound.
- **Fencing failure:** two runners both writing to one run in a supported storage backend.

## What doesn't count

- What a model says or decides. agentsafe can't make a model truthful; it makes what the model does checkable and safe to retry. A model that proposes a wrong payment which your `Check` doesn't catch is a gap in that `Check`.
- Bugs in your own tools, your payment provider, or your systems of record (though `tooltest.SameKey` will help you find them).
- Key management: keeping `FileLog.Key` and `Codec` keys away from the log's host is your deployment's job.
- Anything requiring an attacker who can already run code as the agent's process user.
- Versions other than the latest release.

## Supported versions

agentsafe is pre-1.0: only the latest release receives fixes. The threat model is in [docs/THREAT_MODEL.md](docs/THREAT_MODEL.md).
