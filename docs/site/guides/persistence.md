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
| S3 object/version/multipart metadata and bytes | Metadata reloads from SQLite; immutable files remain in `blobs/` |
| DynamoDB items, queue messages, stored stream/log records | Persist with the store |
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
On Docker runtime startup, RDS clears saved endpoints and marks active instances `creating` until the recreated
engine passes its readiness probe. Wait for `available` and read the new endpoint before reconnecting.

## S3 object files

Object and multipart part bytes live in `<storage.path>/blobs/`, outside SQLite and its in-memory record cache.
Memory mode also uses files, in a private temporary directory removed on shutdown. Keep the entire persistent
storage directory together when backing it up, including `blobs/`.

A blob is written to a temporary file, synchronized, renamed to an immutable random file name, and its directory
synchronized before the record reference commits. Overwrites and deletes commit before removing files with no
remaining references. Versions and copies retain their references; open readers and pending commits pin files.
Startup sweeps unreferenced files and interrupted temporary writes. Reset clears references and collects blobs.
Files left by failed commits can remain until reset or restart.

Existing stores are migrated on startup: current objects, noncurrent versions, and multipart parts with embedded
raw bodies are written to files before their records are replaced. Migration is restartable and keeps the SQLite
schema unchanged. Opening an old store still initially loads its embedded bytes into memory; subsequent opens
load only references. Older TinyStack binaries cannot read the new reference records.

HTTP GET and Range GET stream from files. Multipart completion streams files into the final blob while computing
its checksum. REST upload decoding (including aws-chunked uploads) still buffers the request, as do generated
in-process `GetObject`, `Object` helpers and console downloads/previews. Signature verification can retain an
additional upload buffer. The default object/part/completed-object limit is 256 MiB, configured by
`storage.s3_max_object_bytes` (1 byte through 5 GiB). Raising it increases upload memory requirements; multipart
uploads with small parts avoid buffering the whole object. Other services retain their existing REST limits.
