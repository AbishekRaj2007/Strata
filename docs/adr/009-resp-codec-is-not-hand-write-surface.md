# ADR-009: The RESP codec is not hand-write surface

**Status:** Accepted

## Context

plan.md §4 lists six components that must be hand-written: the skip list, the bloom filter, the block encoder and decoder, the k-way merge iterator, the compaction picker and executor, and WAL record framing and recovery. The stated reason is that these are the project's interview surface — the parts that must be derivable at a whiteboard, because the project's value is what the author can explain under questioning.

The RESP2 codec (T1.1) was being treated as a seventh member of that list. That reading came from CLAUDE.md, from `internal/resp/CONTRACT.md`, and from T1.1's own status note — not from §4 itself, which never names it. The nearest listed item is "WAL record framing and recovery," which is a different framing problem: WAL framing is a durability construct whose failure mode is silent data loss after a crash, while RESP framing is a wire protocol whose failure mode is a rejected client connection.

Holding T1.1 to the hand-write rule had a concrete cost. Every remaining Phase 1 task — T1.2, T1.3, T1.4 — was written, compiled against the codec API, and blocked behind it. The tree did not build. A task whose difficulty is well-understood was gating three tasks whose difficulty is not.

## Decision

The RESP2 codec is removed from the hand-write set. AI may implement `internal/resp/resp.go` against the surface recorded in `internal/resp/CONTRACT.md` and the behaviour asserted by `internal/resp/resp_test.go`.

The six components enumerated in §4 are unchanged. This ADR narrows one item that was being read into that list; it does not narrow the list.

The test suite and fuzz target remain author-written. That ordering is what keeps the change honest: the specification of correct behaviour — 29 malformed inputs, the null-versus-empty distinction, the clean-EOF-versus-truncation distinction — was authored before any implementation existed, and the implementation is judged against it rather than shipping alongside tests written to match whatever it happened to do.

## Alternatives considered

**Leave T1.1 on the hand-write list.** The status quo, and defensible: a wire protocol codec is genuinely good practice at exactly the framing discipline the WAL will need in Phase 2, and the author would arrive at T2.1 having already made the read-exactly-n-bytes mistake once, cheaply. Rejected because the same lesson is available at T2.1 itself, where it actually carries durability consequences, and because the cost here is three blocked tasks and a tree that does not compile.

**Remove the hand-write rule entirely.** Rejected outright. The six components are the project's substance. A storage engine whose compaction executor was generated is, in plan.md's own words, worth less than no storage engine at all. The rule survives this ADR intact.

**Hand-write the framing helpers, generate the rest.** Split `readLine` and the length-prefix parser to the author, generate the type dispatch and the writer. Rejected as the worst of both: it preserves the block on Phase 1 while producing a file with two authors and no clean line in the README about which is which.

## Consequences

**Accepted:** the author does not write the RESP framing loop, and therefore does not arrive at T2.1's WAL framing having practised on a lower-stakes version of the same problem. T2.1 is where that skill now has to be built, against a harder failure mode. This is the real cost of this decision and it is not zero.

**Accepted:** the README's AI-assistance disclosure now includes the RESP codec by name. plan.md §4's honesty rule is explicit that partial claims beat total ones, and "I hand-wrote the storage engine; the wire protocol codec was AI-assisted" is the sentence this ADR obligates.

**Accepted:** a question about RESP framing in an interview is now a question about code the author reviewed rather than code the author wrote. The mitigation is that the test suite is author-written, so the *specification* of correct framing behaviour — including every malformed case — is defensible even where the implementation is not.

**Gained:** Phase 1 unblocks. T1.2, T1.3, and T1.4 can be verified against a real codec rather than the throwaway reference that was deleted, and the tree builds again.

**Gained:** T0.1 can close. Its done-when condition is green CI on a push, which was unreachable while `go build ./...` failed on eight undefined symbols in `internal/server`.

**Follow-up:** T1.1's done-when condition is unchanged — 60 seconds of `FuzzReadValue` without a panic, and the full malformed table passing. An AI-written implementation does not lower that bar, and CI now enforces the fuzz condition on every push rather than leaving it to a manual run.
