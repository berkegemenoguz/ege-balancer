# Day 2 — Config module

**Planned deliverable:** validated configuration loading.

## What was built

- `internal/config` with the full schema from section 3.3 as Go types, and string enums for the
  algorithm, failure policy, log level and log format.
- `Load(path)`, applying defaults and then validating.
- A `Duration` type that parses `5s` and `1m30s`.
- Unit tests at 91.2% coverage.

## Decisions

**A custom `Duration` type.** yaml.v3 decodes into `time.Duration` only from an integer count of
nanoseconds, so `interval: 5s` would have failed. The alternative was writing durations as
integers in the configuration file, which reads badly and invites unit mistakes.

**Validation reports every problem at once, using `errors.Join`.** Returning the first error
means fixing a configuration file one restart at a time. A single run now lists everything that
is wrong.

**Unknown fields are rejected** through `decoder.KnownFields(true)`. A typo such as `algoritm`
would otherwise be ignored silently and the balancer would start with a default nobody asked
for.

**The shipped example configuration is loaded by a test.** If the schema and the example ever
drift apart, CI fails rather than a user discovering it.

## What went wrong

**A test fixture was appended to the wrong place.** The duplicate backend case added a backend
entry to the end of the file, which by then was inside the `logging:` block, so YAML parsing
failed instead of validation. The fixture now inserts the duplicate directly after the existing
backend entry. The bug was in the test, not the code, but it looked like a parser failure at
first.

**staticcheck flagged a `switch` with a single condition** (QF1002) where an if/else reads
better. Fixed before pushing.

## Verification

Running the binary against the example config printed the loaded summary; running it against a
deliberately broken file printed fifteen distinct problems at once and exited non-zero.

## Process change

Section 7.1 of the design document prescribes protected `main` with feature branches and pull
requests. For a single developer that is overhead without benefit, so work goes straight to
`main` and CI runs on every push instead of on pull requests. The design document's two
sentences on branching no longer describe the process.
