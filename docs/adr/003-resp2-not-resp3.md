# ADR-003: RESP2, not RESP3

**Status:** Accepted

## Context

Strata speaks the Redis wire protocol so that existing clients, `redis-cli`, and `redis-benchmark` work unmodified. That decision buys an entire client ecosystem for the cost of one parser, and it is why the project can be benchmarked with standard tools rather than a bespoke harness whose numbers nobody trusts.

Redis 6 introduced RESP3, which adds typed replies — maps, sets, doubles, booleans, big numbers, verbatim strings — and push messages for client-side caching. A protocol version has to be chosen before T1.1.

## Decision

RESP2 only. `HELLO 3` is answered with an error, which causes conforming clients to fall back to RESP2.

## Alternatives considered

**RESP3.** Richer type information and the protocol Redis itself now prefers. Rejected because none of its additions serve a string-only command set: with no hashes, sets, or sorted sets in scope (plan.md §2 non-goals), there is nothing to return as a map or a set. The push-message machinery only pays off for client-side caching, which is not in scope either. It would be parser complexity in exchange for capability the command set cannot use.

**Both, negotiated via `HELLO`.** The complete solution, and what Redis does. Rejected as scope that buys nothing here: it requires per-connection protocol state and a second encoder for the same replies. If the command set ever grows past strings, this becomes the right answer.

**A custom binary protocol.** Would be smaller and faster to parse. Rejected outright — it would forfeit the entire client ecosystem and, more importantly, the ability to benchmark with `redis-benchmark`. Published numbers from a standard tool are worth more than a marginally faster protocol nobody can point a client at.

## Consequences

**Accepted:** clients requesting RESP3 via `HELLO 3` get an error reply. This is the documented fallback path and well-behaved clients handle it, but it must be tested against real client libraries in T1.3 rather than assumed.

**Accepted:** every reply is a simple string, error, integer, bulk string, array, or null bulk string. Anything richer has to be encoded into one of those, which constrains what `INFO` can report to a text blob.

**Accepted:** no client-side caching or server push, ever, without revisiting this ADR.

**Gained:** five types and one null form make a parser small enough to fuzz exhaustively and reason about completely, which is what T1.1's fifteen-malformed-input requirement depends on.

**Critical detail:** the null bulk string `$-1\r\n` and the empty bulk string `$0\r\n\r\n` are distinct values and must stay distinct through every layer. Null means the key does not exist; empty means the key exists with a zero-length value. Collapsing them makes deleted keys read back as empty strings — a correctness bug that would surface in Phase 4 and be traced back to this decision.
