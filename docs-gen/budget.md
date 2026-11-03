## `puff budget`

Braket spending limits, wrapped: puff sets, lists and removes them, and
Braket enforces them. A limit is per QPU, in the QPU's region. Braket
checks each new task's estimated cost against the remaining budget (limit
minus current spend minus queued spend), and rejects a task that doesn't
fit, or any task outside the limit's --from/--until period. Only tasks
submitted while a limit exists count against it.

Spending limits cover QPU tasks only: not simulators, notebooks, Hybrid Job
instances or Braket Direct reservations (Amazon Braket Developer Guide).

Setting or removing a limit isn't billed. puff leaves a limit it didn't
create alone unless --include-others is given.

```
puff budget
```

### `puff budget ls`

List every spending limit in every Braket region: the limit, Braket's
current and queued spend against it, the remaining budget, and its period.
The spend figures are Braket's spending-limit accounting (the estimated cost
of tasks submitted while the limit exists), not a bill. A region that fails
is named, and the command exits 1 after listing the rest.

```
puff budget ls
```

### `puff budget rm`

Remove the spending limit from QPUs

```
puff budget rm [<device>...] [flags]
```

**Examples:**

```sh
puff budget rm cepheus
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--all-qpus` |  | bool |  | Every QPU that isn't RETIRED, in every region |
| `--include-others` |  | bool |  | Also change limits puff didn't create |

### `puff budget set`

Set a spending limit on QPUs, creating or updating it

```
puff budget set [<device>...] --usd <amount> [flags]
```

**Examples:**

```sh
puff budget set cepheus --usd 1
  puff budget set --all-qpus --usd 5 --until 2026-12-31   # through Dec 31, UTC
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--all-qpus` |  | bool |  | Every QPU that isn't RETIRED, in every region |
| `--from` |  | string |  | Start of the period (default now; needs --until) |
| `--include-others` |  | bool |  | Also change limits puff didn't create |
| `--project` |  | string | `default` | puffball:project tag on limits puff creates |
| `--until` |  | string |  | End of the limit's period: a date (through that day, UTC) or an RFC 3339 time; outside the period Braket rejects every task |
| `--usd` |  | string |  | The limit in US dollars (e.g. 1 or 2.50; 0 blocks every task) |

