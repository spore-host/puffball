## `puff ledger`

Estimate what quantum tasks cost, from every Braket region: all tasks, not
only puff's. The ledger is rebuilt from the service each time. Each task is
priced from its metadata with the embedded price table (QPU: per-task fee plus
shots x per-shot, one fee per program set; SV1 and DM1: the billed duration in
the task's results.json, with the 3 s minimum).

Completed tasks make the total. Pending tasks and cancelled or failed ones
are shown apart and never added in. A task on a device the price table
doesn't price is named, with no figure, and the command exits 1. So does a
region that couldn't be searched, after the rest is shown.

The only local state is a cache of simulator durations already read from S3
($XDG_STATE_HOME/puffball/ledger); deleting it loses nothing.

```
puff ledger [flags]
```

**Examples:**

```sh
puff ledger
  puff ledger --by device --since 168h
  puff ledger --by day --tasks --json
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--by` |  | string | `project` | Group by project, device, rung or day (UTC creation date); untagged tasks group as "untagged" |
| `--no-cache` |  | bool |  | Neither read nor write the local cache of simulator durations |
| `--since` |  | duration | `720h0m0s` | Only tasks created within this long (0: all) |
| `--tasks` |  | bool |  | Also list each task with its arithmetic |

