# CLAUDE.md — puffball

`puffball` (command `puff`) is the spore.host tool for Amazon Braket: a job,
ledger and provenance layer. It discovers devices in every Braket region, prices
work before it runs, submits OpenQASM 3 and AHS programs, tracks tasks and jobs
across regions, keeps an account-side spend estimate, wraps Braket spending
limits, and writes a provenance record for every hardware result. It does not
author circuits. Part of the spore.host suite
([spawn](https://github.com/spore-host/spawn),
[truffle](https://github.com/spore-host/truffle),
[lagotto](https://github.com/spore-host/lagotto), puffball).

The design is the pinned `design` issue. Status lives in the GitHub Project,
milestones G0–G3, issues and labels — nowhere else.

## Versioning & changelog (required)

This project follows **[Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html)**
and keeps a **[Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/)**-format
`CHANGELOG.md` at the repo root.

- The first release is **0.1.0**. puffball stays on 0.x until Scott says 1.0.0;
  nothing (release tooling, config, docs) moves it there. Pre-1.0, breaking
  changes bump MINOR.
- **Claude Code creates no tags or releases and chooses no version numbers.**
- Every user-facing change adds an entry under `## [Unreleased]` in the right
  group (`Added`, `Changed`, `Deprecated`, `Removed`, `Fixed`, `Security`, or
  `Documentation`) in the **same PR**. Describe the user-visible effect.
- Enforced: CI fails a PR that changes non-test Go source without touching
  `CHANGELOG.md`; `changelog_test.go` checks the format. `make changelog-fix`
  merges duplicate groups.
- On release (Scott): promote `[Unreleased]` to `[X.Y.Z] - YYYY-MM-DD`, open a
  fresh `[Unreleased]`, update the compare links, tag `vX.Y.Z`. GoReleaser runs
  on the tag.

## Go standards

- Go 1.27, modules, no cgo. cobra. aws-sdk-go-v2 (`service/braket`,
  `service/s3`, `service/pricing`, `service/costexplorer`). Python only in
  `python/puff_local.py`, embedded with `go:embed` and run through `uv`.
- `gofmt`/`goimports`; pass `go vet`, `staticcheck`, `golangci-lint`.
- Godoc comments on all exported identifiers. No `panic` except unrecoverable
  init failures.
- Short idiomatic names; wrap errors `fmt.Errorf("op: %w", err)`; return early;
  stdlib over dependencies; imports grouped stdlib, external, internal.

## CLI patterns

- cobra; flags over args when more than one input.
- Exit codes: 0 success, 1 error, 2 usage error.
- stdout for output only; stderr for errors, progress and logs, so `--json`
  pipes cleanly. `--json` on every read command.

## AWS SDK

- Load config through `pkg/awscfg` (shared spore.host resolution:
  flag > `SPORE_*`/`AWS_*` env > `~/.config/spore/config.toml`).
- Always pass a context. Wrap SDK errors with the operation. Use paginators.
- AWS responses use pointer fields AWS may leave nil; guard before dereferencing.
- Execution in this project: `AWS_PROFILE=aws`.

## Testing

- Coverage floor 60% (CI-enforced over shipped packages), target 80%+.
- Table-driven tests; `t.Helper()` in helpers; test error paths.
- Mock AWS with interfaces (fakes in `pkg/testutil`), not SDK mocks.
- Fixtures in `testdata/`; golden files for `--json` output and records.

## Security

- Never log credentials or tokens. Validate external input. No compiled
  binaries committed.

## Git & GitHub

- `gh` for all GitHub operations. Conventional commits (`feat:`, `fix:`,
  `refactor:`, `test:`, `docs:`, `chore:`); branches `feat/`, `fix/`,
  `refactor/`, `test/`, `docs/`, `chore/`.
- Cut every branch from `origin/main` with a clean tree. One topic per PR.
  Link the issue (`Fixes #N`).
- PRs only. Claude Code may squash-merge a PR that touches only `docs-gen/`,
  `testdata/`, `scripts/`, `.github/`, `.claude/` and `*_test.go` once CI is
  green. Anything touching `cmd/`, `pkg/` or `python/` waits for Scott.
- Record decisions and corrections as issue comments.

## Project tracking

GitHub is the single source of truth: Projects for the board, milestones
(G0–G3) for gates, issues for each unit of work, labels for type, priority and
component. No plan, status, roadmap or TODO files. Allowed local files: README,
LICENSE, CHANGELOG, CLAUDE.md, CITATION.cff, code, tests, `testdata/`,
`scripts/`, `docs-gen/`, `.github/`, `.claude/`.

## Fixed decisions for this repo

- **Hardware spend.** G0–G1 use only Rigetti Cepheus-1-108Q (us-west-1) and
  SV1. No IonQ, AQT, IQM or Aquila task unless Scott names the device in an
  issue. Smallest shot count that answers the question.
- **Results buckets.** The SDK default name per region,
  `amazon-braket-<region>-<account>`, results under `puffball/`. Use it if
  present; create it only on a `--real` run if absent, and report that you did.
  No other buckets.
- **Account settings.** Don't change any (cost allocation tag activation
  included). File an issue for Scott.
- **Regions.** Whatever `SearchDevices` returns; never hard-code the list.

## Laws

1. **Nothing billable without `--real`.** The local simulator is the default
   rung. Every paid action prints its arithmetic first. Billable operations are
   Braket `Create*` task and job calls and every Cost Explorer request ($0.01
   each). Only `pkg/gate` calls billable operations; a test with a fake client
   that fails on any billable call proves it.
2. **Estimates are labelled and dated.** Every dollar figure says "estimate"
   and carries the price table's as-of date. Unknown price → error, never a
   guess or zero.
3. **All regions, always.** List, ledger, budget and sweep cover every region
   `SearchDevices` reports. A failing region is named in output and exit code.
4. **The service is the truth.** The ledger is rebuilt from service state; the
   local cache can be deleted without loss.
5. **Wrap, don't duplicate.** Use Braket's spending limits, program sets, tags
   and queue info; never reimplement them.
6. **Records are complete or flagged.** Missing fields are named in
   `missing[]`, never omitted silently.
7. **No account numbers** in code, fixtures, issues, PRs or posted logs.
   Fixtures pass `scripts/scrub-fixtures`; gitleaks runs in CI.
8. **Spend hygiene.** Cepheus and SV1 only in G0–G1; stop limits per gate;
   `make sweep-check` after every session.
9. **Measured, not assumed.** API fields, price coverage and the program-set
   schema come from live responses or `amazon-braket-schemas-python`, never
   memory. Show a live response containing a field before relying on it.
10. **Authority.** Scott decides scope, names, versions, tags, releases, merges
    of `cmd/`, `pkg/`, `python/`, and when anything is done. Report state and
    outcome; never declare completion.
11. **No advocacy.** Docs state mechanisms, commands and numbers.

## Build & test

- `make check` — fmt gate, vet, lint, short tests (run before every commit)
- `make test` — all tests
- `make build` — `bin/puff`
- `make gen-docs` — regenerate `docs-gen/` (CI fails if stale)
