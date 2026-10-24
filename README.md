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
| `puff ledger` | Estimated spend from the account side, grouped by project, device, rung or day |
| `puff budget set\|ls\|rm` | Braket spending limits, each set in its device's region; `--all-qpus` |
| `puff record <task>` | Provenance record: device, calibration snapshot, compiled program, counts, cost, software versions; missing fields named |
| `puff doctor` | Checks credentials, regions, results buckets and `uv` |
| `puff version` | Version, commit, build date |

Every read command takes `--json`.

## Regions

Braket devices live in different regions (today us-east-1, us-west-1 and
eu-north-1). `puff` discovers the set from `SearchDevices` at run time and
covers all of them. A region that fails is named in the output and the exit
code.

## Tags

Everything `puff` submits carries `puffball:project`, `puffball:rung`,
`puffball:run-id` and `puffball:est-usd`. The ledger also counts untagged tasks,
under `untagged`.

## License

Apache 2.0. See [LICENSE](LICENSE). To cite puffball, see
[CITATION.cff](CITATION.cff).
