## `puff doctor`

Check what puff needs, and say how to fix what's missing:
  - AWS credentials resolve;
  - every Braket region answers;
  - each region's default results bucket exists (reported, never created);
  - the client-token directory is writable;
  - uv is installed and the pinned simulator is in uv's cache, so a local
    run works offline;
  - the price table is current and matches the AWS Price List;
  - the third-party device user agreement, per QPU region, as far as service
    state shows it: accepted when Braket has accepted a QPU task in the region
    (the newest in 90 days, else any), unknown otherwise. This assumes the
    agreement covers every QPU provider in a region, which hasn't been
    measured; the detail names the device whose task is the evidence.

On AWS it only reads: STS, Braket searches, S3 HeadBucket, and the Price List
API (none billed). It calls no Cost Explorer and changes nothing in the
account. Locally it creates the client-token directory if it is missing.
Exits 1 if a check fails, including a region or the Price List that can't
be read.

```
puff doctor
```

