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

[Unreleased]: https://github.com/spore-host/puffball/commits/main
