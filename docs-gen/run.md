## `puff run`

Run an OpenQASM 3 program on the Braket default simulator, on this machine.

This is the local rung: no AWS call, no credentials, not billable. It runs
the pinned amazon-braket-default-simulator on Python 3.14 through uv
(fetched once, then cached). With --offline, uv uses only its cache and
never the network.

Running on Amazon Braket (--device, --real) arrives with #39.

```
puff run <file.qasm> [flags]
```

**Examples:**

```sh
puff run bell.qasm --shots 1000
  puff run bell.qasm --shots 1000 --offline --json
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--offline` |  | bool |  | Use only uv's cache; never the network |
| `--shots` |  | int64 | `1000` | Shots |

