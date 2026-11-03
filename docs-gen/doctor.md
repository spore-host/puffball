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
    state shows it.

It reads only: STS, Braket searches, S3 HeadBucket, and the Price List API
(none billed). It calls no Cost Explorer, creates nothing and changes nothing.
Exits 1 if a check fails.

```
puff doctor
```

