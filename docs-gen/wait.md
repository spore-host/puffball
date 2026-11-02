## `puff wait`

Poll a quantum task until it is COMPLETED, FAILED or CANCELLED, printing each
change of status and queue position to stderr. Exits 0 when the task
completes, 1 when it fails, is cancelled, or is still running at --timeout.
Ctrl-C stops waiting; it does not cancel the task.

```
puff wait <task-id|task-arn> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--interval` |  | duration | `10s` | Time between polls |
| `--timeout` |  | duration | `30m0s` | Stop waiting after this long |

