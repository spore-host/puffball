## `puff price`

Print the pricing-page arithmetic for running programs on a device.

The figure is an estimate (the price table's arithmetic, as of its date). It is
not a bill: billing arrives later and can differ. Nothing is submitted and no
AWS call is made.

QPUs:       $0.30 per task + shots x the device's per-shot price.
--pack K:   on gate-based QPUs, packs up to K programs into one program set,
            which pays one per-task fee.
SV1, DM1:   per minute of billed duration, with a 3 s minimum per task. The
            duration isn't known before a run; without --duration the
            estimate is the minimum.

```
puff price <device> [flags]
```

**Examples:**

```sh
puff price cepheus --shots 1000
  puff price cepheus --shots 1000 --tasks 500 --pack 5
  puff price sv1 --duration 10s
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--duration` |  | duration | `0s` | Expected billed duration per task, for SV1 and DM1 (e.g. 10s) |
| `--pack` |  | int |  | Pack up to this many programs into each program set (gate-based QPUs) |
| `--shots` |  | int64 |  | Shots per program |
| `--tasks` |  | int | `1` | Number of programs (each a task, unless --pack) |

