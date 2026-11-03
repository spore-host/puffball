# Changelog

All notable changes to **puffball** (`puff`) are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- **`puff version`** prints the version, commit and build date, and checks
  GitHub for a newer release; `--json` prints the same fields as JSON.
- **Global flags** shared with the other spore.host tools: `--profile` and
  `--region` (resolved flag > `SPORE_*`/`AWS_*` env > `~/.config/spore/config.toml`),
  `--json`, `--verbose`, `--lang`, `--no-emoji`, `--accessibility`.
- **Exit codes** 0 (success), 1 (error), 2 (usage error).
- **`puff price <device>`** prints the estimated cost of a run before anything
  runs: `$0.30` per task plus shots × the device's per-shot price on QPUs, and
  per-minute billing with a 3 s minimum on SV1 and DM1. `--tasks`, `--pack` (one
  per-task fee per program set, gate-based QPUs only) and `--duration` are
  supported. It is offline (no AWS call, no credentials). Every figure is
  labelled an estimate with the price table's date (2026-10), and a warning
  appears once the table is more than 60 days old. An unpriced device is an
  error. `--json` is supported.
- **`puff devices`** lists every Amazon Braket device in every region (from the
  public SSM region list): status, qubits, paradigm, queue depth, whether an
  execution window is open (or when the next one opens), calibration age, and
  the estimated cost of one 1,000-shot task. Qubit counts and status come from
  the service. Devices whose capabilities carry no calibration time (IQM,
  QuEra) show "none", with no substitute. `--all` adds retired devices, with
  their replacement where one is known; `--paradigm gate|analog` filters. A
  region that fails is named and the command exits 1, but the rest is still
  listed. `--json` is supported.
- **`puff run <file.qasm>`** runs an OpenQASM 3 program on the Braket default
  simulator on this machine, the local rung. It makes no AWS call, needs no
  credentials, and costs nothing. It runs the pinned
  `amazon-braket-default-simulator` 1.41.1 through `uv`, which fetches it once
  and then caches it. `--offline` uses only uv's cache. Prints counts, or
  `--json`.
- **`puff run <file> --device <alias>`** runs on Amazon Braket. The device is
  found in the live service, the run is priced, and the estimate is printed
  with its price date. Nothing is submitted without `--real`; `--dry-run` and
  `--budget` stop it too. With `--real` the task goes to the device's region,
  with results in that region's default bucket
  (`amazon-braket-<region>-<account>`, created on a `--real` run if missing,
  and reported) under `puffball/<run-id>/`. Tasks are tagged
  `puffball:project` (`--project`), `puffball:rung`, `puffball:run-id` and
  `puffball:est-usd`, plus any `--tag key=value`. Repeating `--run-id` retries
  the same request and never creates a second task. OpenQASM 3 (`.qasm`) runs
  on gate devices and AHS (`.json`) on analog ones. For SV1 and DM1 the
  estimate is the minimum charge, and `--budget` needs `--duration`; with
  `--device-region` you choose the region. `--json` is supported.
- **`puff run --device ... --real`** also stores two objects beside the
  results, under `puffball/<run-id>/submission/<alias>/`: the action JSON as
  submitted, and the device's capabilities as read just before submission.
  `puff record` uses them. They are never overwritten, so a retry keeps the
  first attempt's. If they can't be stored, the run warns and goes on.
- **`puff ls`** lists quantum tasks in every Braket region, including tasks
  that puff didn't submit (their project and run id show `-`). Filters:
  `--state` (repeatable), `--since` (default 30 days; `0` for all),
  `--project`, `--device` and `--mine` (only tasks puff submitted). If a
  region fails, it is named, the other regions are still listed, and the
  command exits 1. `--json` is supported and reports `complete` and
  `regions_failed`.
- **`puff get <task>`** shows one task: status, queue position while it
  waits, failure reason, device, shots, program count, times, project, run id
  and results location. Takes a task ID or a task ARN. An ID is searched for
  in the same regions `ls` covers; if it isn't found and a region couldn't be
  searched, that region is named. `--json` is supported.
- **`puff wait <task>`** polls a task until it finishes and prints each change
  of status and queue position to stderr. Exits 0 when the task completes,
  and 1 when it fails, is cancelled, or `--timeout` (default 30m) passes.
  `--interval` sets the time between polls (default 10s). A failed poll is
  reported and retried, and three in a row end the wait. Ctrl-C stops waiting
  but doesn't cancel the task. With `--json`, the last status is printed even
  when the wait fails.
- **`puff cancel <task>`** cancels an unfinished task and prints the status
  the service reports. Cancelling is not billed. A task that has already
  finished, or finishes before the cancel arrives, is left alone and reported
  as such. `--json` is supported.
- Account IDs in task ARNs, results locations and service error text are shown
  as `<account>` in all four commands.
- **`puff ledger`** estimates spend on quantum tasks in every Braket region:
  all tasks, not only puff's. It is rebuilt from the service each time.
  - **Pricing:** each task is priced with the embedded table. On QPUs that is
    the per-task fee plus shots × the per-shot price, with one fee per program
    set. SV1 and DM1 are billed for the duration recorded in the task's
    `results.json` in S3, with the 3 s minimum; "at least" marks a minimum
    charge where no duration was read.
  - **Grouping:** `--by project|device|rung|day`, with untagged tasks under
    `untagged`. `--since` defaults to 30 days (`0` for all). `--tasks` lists
    each task with its arithmetic.
  - **Totals:** completed tasks make the total. Pending tasks (created,
    queued, running, cancelling) are shown in their own column and not
    added in until they finish. Cancelled and failed tasks are listed at $0,
    whatever their device: Braket adds nothing to an account's current
    spend for them (Amazon Braket Developer Guide, "Cost tracking and
    saving").
  - **Exit 1:** for a task with no figure (a device the table doesn't price,
    or a QPU task the service reports no shots for; each is named), or for a
    region that couldn't be searched. In that case the output is headed
    `INCOMPLETE`, the row reads `TOTAL (incomplete)`, and the other regions
    are still shown.
  - **Cache:** durations already read from S3 are cached under
    `$XDG_STATE_HOME/puffball/ledger`, keyed by region and task ID (no account
    ID). Deleting the cache loses nothing, and `--no-cache` skips it.
  - Every figure is labelled an estimate with the price date. `--json` is
    supported.
- **`puff record <task>`** writes the provenance record for a task's
  result as `puffball.record/v0` JSON, to stdout or `--out` (an existing file
  isn't overwritten without `--force`). The record holds:
  - device, program sources, shots and counts (each program and executable
    of a program set);
  - the physical qubits and compiled program (Quil, or OpenQASM 3 for
    program sets);
  - the device's calibration for those qubits at submission and at
    completion;
  - the estimated cost with its price date, software and result-schema
    versions, and tags;
  - the S3 key, ETag and SHA-256 of every object it read.

  Anything it couldn't fill is named in `missing[]` with the reason, and
  listed on stderr. The calibration at completion counts only if no
  calibration of the task's qubits is timestamped after the task ended,
  and nothing changed since the submission snapshot. Otherwise the later
  calibration is kept as `at_record`, labelled. A snapshot taken more than
  15 minutes before the task was created (left by an earlier, failed
  attempt) isn't used as the calibration at submission. Account IDs appear as
  `<account>`. The JSON Schema ships in `pkg/record/schema/`.
- **`puff budget`** sets, lists and removes Amazon Braket spending limits on
  QPUs. Braket enforces them: a task whose estimated cost exceeds the
  limit's remaining budget, or one submitted outside its period, is
  rejected.
  - `puff budget set <device>... --usd N` creates the device's limit, or
    updates the one it has, in the device's region. `--all-qpus` covers
    every QPU that isn't retired, and is refused if a region couldn't be
    searched. `--from`/`--until` set the period; `--project` tags it.
  - `puff budget ls` lists limits in every region, with Braket's spent and
    queued figures and the remaining budget. These are Braket's own
    accounting, not a bill. A failed region is named, and the command exits
    1.
  - `puff budget rm <device>...` (or `--all-qpus`) removes limits.
  - A limit puff didn't create is left alone unless `--include-others` is
    given.
  - Spending limits cover QPU tasks only, so simulators are refused.
    Setting or removing a limit isn't billed. `--json` is supported.
- **`puff record verify <file>`** checks a record offline against the
  schema and recomputes the SHA-256 of its sources, compiled programs and
  calibration subsets. Exit 1 lists each problem; `--json` is supported.

### Changed
- **Repo tooling only, no change to `puff`:** G0 API probes (`make probe`,
  `make probe-paid REAL=1`, `make fixtures`, `make sweep-check`), the fixture
  scrubber, and scrubbed device, task and Price List fixtures under `testdata/`.
- **Internal, no new command yet:** the Braket client layer that the coming
  commands build on (#34). It discovers regions from the public SSM Braket
  region list and searches every region. A region that fails, including one
  that fails part-way through a listing, is reported by name instead of being
  left out. It also builds OpenQASM 3, program-set and AHS action JSON.
- **Internal, no new command yet:** the gate every billable command will go
  through (#35). It prints each estimate with its price date, stops unless
  `--real` is given, honours `--dry-run` and `--budget`, and reuses a stored
  client token on retry. Braket returns the same task for a repeated token
  (measured), so a retried run can't be billed twice. A test fails the build
  if anything outside the gate makes a billable call.

### Fixed
- A task or job ID whose last group is all digits (about 1 UUID in 280) is no
  longer mistaken for an account ID and printed as `000000000000` in errors
  and scrubbed fixtures.

[Unreleased]: https://github.com/spore-host/puffball/commits/main
