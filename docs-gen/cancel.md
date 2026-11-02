## `puff cancel`

Cancel a quantum task that hasn't finished. Cancelling isn't billed (it isn't
one of puff's billable operations) but it changes the task for good, so puff
reads the task first and says what it is cancelling. A task that has already
finished is left alone.

```
puff cancel <task-id|task-arn>
```

