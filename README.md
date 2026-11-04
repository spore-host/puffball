# puffball

`puff` is the [spore.host](https://spore.host) tool for Amazon Braket: a job,
ledger and provenance layer over the Braket API. It discovers devices in every
Braket region, prices work before it runs, submits OpenQASM 3 and AHS programs,
tracks tasks across regions, keeps an account-side spend estimate, wraps Braket
QPU spending limits, and writes a provenance record for every hardware result.

It does not author circuits. Circuits come from the Braket SDK, Qiskit-Braket,
PennyLane or CUDA-Q, as OpenQASM 3 or AHS programs.

## The `--real` rule

Nothing billable happens without `--real`. The default target is the local
simulator, which needs no AWS credentials. Every paid action prints its cost
arithmetic (labelled as an estimate, with the price table's as-of date) before
the call, for example:

```
estimate (prices as of 2026-10): $0.30 + 1000 x $0.000425 = $0.725
```

Without `--real` the command stops there.

## Install from source

Requires Go 1.27 or later. The local simulator rung also needs
[`uv`](https://docs.astral.sh/uv/).

```
git clone https://github.com/spore-host/puffball
cd puffball
make build        # bin/puff
```

## Commands

The v0.1 command surface. `puff --help` lists the commands in the build you
have; the generated reference is in [`docs-gen/`](docs-gen/).

| Command | Does |
| --- | --- |
| `puff devices` | Every device in every Braket region: status, qubits, paradigm, execution windows, queue depth, calibration age, price per 1,000 shots |
| `puff price <device> --shots N` | Cost arithmetic before anything runs, including `--tasks N` and program-set packing (`--pack K`) |
| `puff run <file> --device <alias> --shots N` | Submit an OpenQASM 3 or AHS program; local simulator unless `--device` and `--real` |
| `puff ls`, `get`, `wait`, `cancel` | Tasks in every region, whether or not `puff` submitted them |
| `puff ledger` | Estimated spend from the account side, every task in every region, grouped by project, device, rung or day; cancelled and failed tasks at $0; Hybrid Jobs, notebooks, S3 and Braket Direct not yet included |
| `puff budget set\|ls\|rm` | Braket spending limits, each set in its device's region; `--all-qpus` |
| `puff record <task>` | Provenance record (`puffball.record/v0`): device, program, counts, physical qubits, compiled program, calibration at submission and completion, cost, software versions, S3 object hashes; missing fields named. `puff record verify <file>` checks one |
| `puff doctor` | Checks credentials, every region, results buckets, the client-token directory, `uv` and the cached simulator, price-table drift against the Price List, and the third-party device agreement |
| `puff version` | Version, commit, build date |

Every read command takes `--json`.

## A worked example: GHZ-3 from the local simulator to Cepheus

A three-qubit GHZ state, from a file on this machine to a recorded result on
Rigetti Cepheus-1-108Q. The output is from the task submitted on 2026-11-02
(#11), with the account ID shown as `<account>`, as `puff` prints it.

`ghz3.qasm` ([testdata/programs/ghz3.qasm](testdata/programs/ghz3.qasm)):

```
OPENQASM 3.0;
bit[3] b;
qubit[3] q;
h q[0];
cnot q[0], q[1];
cnot q[1], q[2];
b[0] = measure q[0];
b[1] = measure q[1];
b[2] = measure q[2];
```

**1. Run it locally.** No AWS call and no credentials; the pinned
`amazon-braket-default-simulator` runs through `uv`.

```
$ puff run ghz3.qasm
ghz3.qasm: 1000 shots on the local simulator (amazon-braket-default-simulator 1.41.1): no AWS call, not billable

OUTCOME  COUNT  FRACTION
111      513    0.513
000      487    0.487
```

**2. Price it.** Offline, from the embedded price table.

```
$ puff price cepheus --shots 1000
Rigetti Cepheus-1-108Q (cepheus)
estimate (prices as of 2026-10): $0.30 + 1000 x $0.000425 = $0.725
This is an estimate, not a bill; nothing was submitted.
```

`puff devices` shows the device's status, queue and execution window (one
row of its output):

```
ALIAS             NAME                REGION      STATUS   PARADIGM  QUBITS  QUEUE  WINDOW            CALIBRATED  ESTIMATE
cepheus           Cepheus-1-108Q      us-west-1   ONLINE   gate      107     10     open              7h-50d ago  $0.725
```

**3. Run it on Cepheus.** Without `--real` it stops after the estimate:

```
$ puff run ghz3.qasm --device cepheus --shots 1000
ghz3.qasm on Cepheus-1-108Q in us-west-1 (the device's region), run id run-04254a5b
estimate (prices as of 2026-10): cepheus: $0.30 + 1000 x $0.000425 = $0.725
Not submitted. Pass --real to spend this.
```

With `--real` it submits, printing `submitting cepheus: estimate $0.725
(prices as of 2026-10)` first, and tags the task. Before submitting it stores
the action and the device's calibration beside the results, for the record.
The same command again, with the same `--run-id`, finds the task instead of
creating a second one:

```
$ puff run ghz3.qasm --device cepheus --shots 1000 --project puffball-g1 --run-id g1-ghz3-record --real
ghz3.qasm on Cepheus-1-108Q in us-west-1 (the device's region), run id g1-ghz3-record
estimate (prices as of 2026-10): cepheus: $0.30 + 1000 x $0.000425 = $0.725
already submitted under this run id (nothing new created): task d9bcbb8d-b346-45f2-a862-0681af012f20 on cepheus in us-west-1
run id:  g1-ghz3-record (repeat it with --run-id to retry without creating a second task)
results: s3://amazon-braket-us-west-1-<account>/puffball/g1-ghz3-record/
tags:    puffball:est-usd=0.725 puffball:prices-as-of=2026-10 puffball:project=puffball-g1 puffball:run-id=g1-ghz3-record puffball:rung=qpu
```

**4. Wait for it.** `puff wait` prints each change of status and queue
position (here from the original run), then the task:

```
$ puff wait d9bcbb8d-b346-45f2-a862-0681af012f20
22:59:31Z d9bcbb8d-b346-45f2-a862-0681af012f20: QUEUED, queue position 1
23:04:19Z d9bcbb8d-b346-45f2-a862-0681af012f20: COMPLETED
```

`puff get` shows it afterwards:

```
$ puff get d9bcbb8d-b346-45f2-a862-0681af012f20
task     d9bcbb8d-b346-45f2-a862-0681af012f20
region   us-west-1
device   Cepheus-1-108Q
status   COMPLETED
shots    1000
created  2026-11-02 22:59Z
ended    2026-11-02 23:04Z
project  puffball-g1
run id   g1-ghz3-record
results  s3://amazon-braket-us-west-1-<account>/puffball/g1-ghz3-record/d9bcbb8d-b346-45f2-a862-0681af012f20/
```

**5. See the spend.** Every task in every region, priced from its metadata:

```
$ puff ledger --since 0 --by device
ESTIMATE by device, all tasks (prices as of 2026-10). An estimate, not a bill.

DEVICE          TASKS  SHOTS  ESTIMATE  PENDING  CANCELLED/FAILED  UNPRICED
Cepheus-1-108Q  4      1400   $1.795    -        -                 -
sv1             5      401    $0.015    -        1 at $0.00        -
TOTAL           9      1801   $1.81     -        1 at $0.00        -
```

(followed by notes on what the figures are and what isn't included yet).

**6. Record it.**

```
$ puff record d9bcbb8d-b346-45f2-a862-0681af012f20 --out ghz3.record.json
wrote ghz3.record.json
record for task d9bcbb8d-b346-45f2-a862-0681af012f20 on Cepheus-1-108Q: 3 S3 objects linked, 2 field(s) missing:
  shots.successful: the service reports successful shots only for some tasks (program sets); not reported for this one
  calibration.at_completion: the device was recalibrated at 2026-11-03T20:39:31Z, after the task ended at 2026-11-02T23:04:04Z; at_record is the later calibration
$ puff record verify ghz3.record.json
ghz3.record.json: verified (schema puffball.record/v0, every recomputable hash matches)
```

This record was written a day after the task ended, and Cepheus had been
recalibrated in between. So the record keeps that later calibration as
`at_record` and names `calibration.at_completion` missing; it doesn't pass
the later calibration off as the one in force. Recording soon after `wait`
returns gives `at_completion` when nothing has changed. Part of the record:

```json
{
  "schema": "puffball.record/v0",
  "task": {"id": "d9bcbb8d-b346-45f2-a862-0681af012f20", "region": "us-west-1", "status": "COMPLETED",
           "created_at": "2026-11-02T22:59:00.827Z", "ended_at": "2026-11-02T23:04:04.115Z", "run_id": "g1-ghz3-record"},
  "physical_qubits": [34, 42, 43],
  "shots": {"requested": 1000, "measured": 1000},
  "counts": {"000": 431, "001": 32, "010": 6, "011": 24, "100": 38, "101": 23, "110": 48, "111": 398},
  "compiled_programs": [{"format": "quil", "sha256": "a742d723baff91659e328d26ac8809489d24db606e1bfe11ffc3926b8104d400", "...": "..."}],
  "calibration": {
    "at_submission": {"source": "puff run, just before submission (puffball/g1-ghz3-record/submission/cepheus/device.json)",
                      "read_at": "2026-11-02T22:58:56.029917Z", "calibrated_to": "2026-11-02T21:24:16Z",
                      "qubits": {"34": "...", "42": "...", "43": "..."}, "pairs": {"34-43": "...", "42-43": "..."}},
    "at_record": {"read_at": "2026-11-03T21:46:09.210307Z", "calibrated_to": "2026-11-03T20:39:31Z", "...": "..."}
  },
  "cost": {"kind": "estimate", "prices_as_of": "2026-10", "estimate_usd": "$0.725",
           "arithmetic": "$0.30 + 1000 x $0.000425 = $0.725"},
  "objects": [{"role": "results", "key": "puffball/g1-ghz3-record/d9bcbb8d-b346-45f2-a862-0681af012f20/results.json", "...": "..."},
              {"role": "submitted-action", "...": "..."}, {"role": "device-at-submission", "...": "..."}],
  "missing": [{"field": "shots.successful", "...": "..."}, {"field": "calibration.at_completion", "...": "..."}]
}
```

The schema is [pkg/record/schema/record-v0.schema.json](pkg/record/schema/record-v0.schema.json).

## Regions

Braket devices live in different regions. `puff` reads the region list at run
time from the public SSM parameter
`/aws/service/global-infrastructure/services/braket/regions` (five regions on
2026-11-03: eu-north-1, eu-west-2, us-east-1, us-west-1, us-west-2), adds any
region a device ARN names, and searches each one: `SearchDevices` answers only
for its own region. A region that fails is named in the output and the exit
code.

## Tags

Everything `puff` submits carries `puffball:project`, `puffball:rung`,
`puffball:run-id`, `puffball:est-usd` and `puffball:prices-as-of`. The ledger
also counts untagged tasks, under `untagged`.

## License

Apache 2.0. See [LICENSE](LICENSE). To cite puffball, see
[CITATION.cff](CITATION.cff).
