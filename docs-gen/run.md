## `puff run`

Run a program.

Without --device: the local rung. An OpenQASM 3 (.qasm) program runs on the
Braket default simulator on this machine: no AWS call, no credentials, not
billable. It runs the pinned amazon-braket-default-simulator on Python 3.14
through uv (fetched once, then cached). --offline uses only uv's cache.

With --device: Amazon Braket. The device is found in the live service, the
run is priced, and the estimate is printed. Nothing is submitted without
--real. With --real it is submitted to the device's region, with results in
that region's default bucket (amazon-braket-&lt;region&gt;-&lt;account&gt;, created on a
--real run if it doesn't exist, and reported), under puffball/&lt;run-id&gt;/.
Every task is tagged puffball:project, puffball:rung, puffball:run-id and
puffball:est-usd. Repeating a run id retries the same request; it never
creates a second task.

SV1 and DM1 bill run time, which isn't known in advance: their estimate is the
minimum, and --budget needs --duration as an upper bound.

```
puff run <file> [flags]
```

**Examples:**

```sh
puff run bell.qasm --shots 1000
  puff run bell.qasm --device cepheus --shots 100            # print the estimate, submit nothing
  puff run bell.qasm --device cepheus --shots 100 --real
  puff run bell.qasm --device sv1 --shots 100 --real --project fmo --tag owner=me
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--budget` |  | float64 |  | Refuse the run if its estimate exceeds this many US dollars (0 refuses any spend) |
| `--device-region` |  | string |  | For a simulator offered in several regions, submit in this one |
| `--device` |  | string |  | Run on this Amazon Braket device (alias, name or ARN) instead of locally |
| `--dry-run` |  | bool |  | Print the estimate and stop, even with --real |
| `--duration` |  | duration | `0s` | SV1/DM1: expected billed duration, for the estimate and --budget |
| `--offline` |  | bool |  | Local: use only uv's cache; never the network |
| `--project` |  | string | `default` | Value of the puffball:project tag |
| `--real` |  | bool |  | Spend money: submit to Amazon Braket (default is to print the estimate and stop) |
| `--run-id` |  | string |  | Run id (default: a new one); repeat one to retry without creating a second task |
| `--shots` |  | int64 | `1000` | Shots |
| `--tag` |  | stringArray |  | Extra task tag key=value (repeatable) |

