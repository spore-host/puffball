## `puff record`

Write the provenance record for a quantum task's result, as
puffball.record/v0 JSON. It holds:
  - the device, program, shots and counts;
  - the physical qubits and compiled program;
  - the device's calibration for those qubits at submission and at completion;
  - the estimated cost (prices as of the table's date), software versions and tags;
  - the key, ETag and SHA-256 of every S3 object it read.
Anything it couldn't fill is named in missing[] with the reason, and listed on
stderr.

The calibration at submission is the snapshot puff run stores beside the
results before it submits. A task puff didn't submit has none. The
calibration at completion is read from the device when the record is made.
It counts only if every calibration timestamp for the task's qubits predates
the task's end; otherwise it is kept as at_record and at_completion is named
missing.

Reads only: GetQuantumTask, GetDevice, S3 GetObject. Account IDs appear as
&lt;account&gt;.

```
puff record <task-id|task-arn> [flags]
```

**Examples:**

```sh
puff record 617b7e2b-f77f-433b-8226-6cc55eb8d077 --out bell.record.json
  puff record verify bell.record.json
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--force` |  | bool |  | Overwrite --out if it exists |
| `--out` | `-o` | string |  | Write the record to this file instead of stdout |

### `puff record verify`

Check a record offline: that it conforms to the puffball.record/v0 JSON
Schema, and that the SHA-256 of every program source, compiled program and
calibration subset matches. The S3 objects' hashes link to the objects and
are not re-fetched. Exits 0 if the record checks, 1 with each problem if not.

```
puff record verify <file>
```

