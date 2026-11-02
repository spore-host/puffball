## `puff ls`

List quantum tasks in every Braket region, whoever submitted them: puff,
the Braket SDK, a notebook, a colleague. Untagged tasks show "-" for project
and run id.

Regions come from the public SSM Braket region list, plus any region a device
ARN names. A region that fails is named, and the command exits 1 after
listing the rest.

```
puff ls [flags]
```

**Examples:**

```sh
puff ls
  puff ls --state queued --state running
  puff ls --since 24h --project fmo --json
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--device` |  | string |  | Only tasks on this device (as shown in the DEVICE column) |
| `--mine` |  | bool |  | Only tasks puff submitted (those with a puffball:run-id tag) |
| `--project` |  | string |  | Only tasks with this puffball:project tag |
| `--since` |  | duration | `720h0m0s` | Only tasks created within this long (0: all) |
| `--state` |  | stringArray |  | Only tasks in this state (repeatable): created, queued, running, cancelling, completed, failed, cancelled |

