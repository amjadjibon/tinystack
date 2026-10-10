# Storage transactions

Services read and write through two callback methods:

```go
type Store interface {
    View(context.Context, func(ReadTx) error) error
    Update(context.Context, func(WriteTx) error) error
}
```

`View` exposes a consistent snapshot. `Update` stages mutations, commits on callback success, and rolls back
on error. Transactions are serializable; byte values are copied and transaction handles stay inside callbacks.
`Keys(prefix)` returns sorted keys, including applicable staged writes.

## Backends

| Backend | Reads | Commit | Restart |
| --- | --- | --- | --- |
| Memory | In-memory records and sorted key index | Atomic in-memory publication | State disappears |
| Persistent | The same in-memory records | SQLite transaction completes before publication | Load records from `state.db` |

Persistent mode is not a disk-only database query engine: the entire record set is reloaded into memory.
The SQLite adapter uses `modernc.org/sqlite`, without cgo. It uses WAL, full synchronization, a schema version,
and an exclusive connection lock. A second server cannot open the same store concurrently.

## Keep I/O outside transactions

Use this sequence for cross-service or runtime work:

```text
View/Update: inspect state and decide the next action
  → leave the transaction
  → invoke another service, HTTP endpoint, or runtime
  → Update: re-read current state and record the outcome
```

Do not perform external I/O inside a callback or call a second store transaction while holding the first.
Background runners can use durable work indexes, cursors, or leases to make retries and restart handling explicit.
For example, Firehose removes buffered records only after the S3 write succeeds.

## Record layout

Most resources are JSON records. DynamoDB items, log events, stream records, S3 body references and other collections
use separate keys. Partition-wide indexes reserve names or locate work across accounts and regions.
This favors small development datasets; independent distributed indexes and capacity simulation are generally absent.

Every backend runs the shared [storage contract suite](https://github.com/amjadjibon/tinystack/tree/main/internal/storage/storagetest).
See the [Store interface](https://github.com/amjadjibon/tinystack/blob/main/internal/storage/store.go),
[SQLite adapter](https://github.com/amjadjibon/tinystack/blob/main/internal/storage/sqlite/store.go), and
[persistence guide](../guides/persistence.md).

## Immutable blob files

The storage layer provides an opt-in immutable file facility, independent of the record store. It owns durable
writes, opaque references, pins, file opens, retirement, collection and temporary-directory cleanup. Owners
supply the live blob IDs for collection and serialize that snapshot with reference publication. Other services
continue to use the original record store without blob bookkeeping.

S3 owns its body record layout and a private transaction helper that tracks overwritten or deleted references.
Counts at `aws/s3/blob/<id>` change atomically with current, historical and multipart references.
After a successful commit, it retires IDs whose counts reached zero, including shared copies and versions,
without scanning records. Readers pin inside read transactions; only writes are ordered through post-commit
retirement, so concurrent reads remain independent. Startup rebuilds counts across accounts and Regions
before migrating legacy bodies. Reset scans body records; orphan collection reads only the S3 count prefix.
File I/O runs outside callbacks; acquiring a pin inside a callback only updates in-memory bookkeeping. S3's startup hook migrates legacy bodies and collects orphan files; its app
reset and shutdown hooks retire body references, collect files and close the file facility. See [persistence](../guides/persistence.md#s3-object-files)
for ordering, migration and buffering limits.

S3 uses `.../db/<key>` for current body references, `.../bb/<version>/<key>` for historical references,
and `.../pb/<upload>/<part>` for multipart references. Startup migrates the old `/d/`, `/b/` and `/p/`
raw-byte records by publishing the file, then atomically moving the record into the reference namespace.
This keeps arbitrary legacy bytes distinct from new reference records.
