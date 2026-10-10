# Persistence and restart behavior

```yaml
storage:
  mode: persistent
  path: ~/.tinystack
```

Save this in a configuration file and pass it with `--config`. Successful storage commits reach `state.db`
before becoming visible. Records reload on restart; only one process can own a database directory.

## Restart matrix

| State | Persistent restart |
| --- | --- |
| Committed resource records and supported stored data | Reload from SQLite |
| S3 objects, DynamoDB items, queue messages, stored stream/log records | Persist with the store |
| Lambda event source mappings | Reload and resume polling |
| Supported durable schedule/retry indexes | Reload; runners resume recorded work |
| IoT retained messages and shadows | Reload |
| Request/delivery logs and gateway metrics | Reset |
| AppSync subscriptions | Closed; clients reconnect |
| MQTT sessions/in-flight packets | Not restored |
| Lambda invocations in flight | Not resumed |
| Step Functions running executions | Aborted on restart; tokens/activity queues are in memory |
| EKS k3s Kubernetes objects | Not retained across restart |
| ElastiCache container data | Lost; metadata reloads and fresh nodes may get new ports |
| RDS SQL data | Retained in named Docker volumes; instance records recreate or reattach containers, with possibly new host ports |

Persistent metadata does not guarantee engine data retention or exactly-once execution. Check
[service-specific limits](../services/index.md) for workflows that cross those boundaries.

## Ownership, backup and reset

Use separate directories for separate servers. Stop the existing owner before reopening a locked store.
An unsupported schema version fails rather than guessing at the format.
For a filesystem backup, stop TinyStack cleanly and copy the directory. SQLite can have WAL files while running;
copying only a live `state.db` does not guarantee a complete snapshot.

Compose stores records in the `tinystack-data` volume. `task down` retains it; `task down -- -v` deletes it.
To reset native state, stop the server and select a new empty directory, keeping the old directory for recovery.
See [storage architecture](../architecture/storage.md).

RDS volumes are separate from SQLite and must be backed up separately. Delete with
`SkipFinalSnapshot=true` or call the reset endpoint to remove recorded containers and volumes.
Shutdown retains RDS volumes; the next Docker runtime removes volumes and containers whose instance records are absent.
