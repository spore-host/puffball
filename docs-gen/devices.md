## `puff devices`

List every Amazon Braket device in every region, from the service:
status, qubits, paradigm, queue depth, execution window, calibration age, and
the estimated cost of one 1,000-shot task (SV1 and DM1: the per-minute rate).

Regions come from the public SSM Braket region list; each is searched. A
region that fails is named, and the command exits 1. Retired devices are
listed with --all, with their replacement where one is known.

Calibration age is from the device's capabilities; IQM and QuEra report no
calibration time, shown as "none" rather than a substitute.

```
puff devices [flags]
```

**Examples:**

```sh
puff devices
  puff devices --paradigm analog
  puff devices --all --json
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--all` |  | bool |  | Include retired devices |
| `--paradigm` |  | string |  | Only gate or analog devices |

