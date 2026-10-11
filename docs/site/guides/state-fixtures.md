# State fixtures and offline export

State fixtures provide portable, deterministic, and verifiable archives containing TinyStack's committed service
records and disk-backed S3 blob files. Fixtures allow sharing reproducible state between local environments and
seeding fast, reusable integration tests in CI.

## Scope and boundaries

State archives capture **committed control-plane service records** stored in SQLite and **referenced S3 object blobs**
stored in the `blobs/` directory.

- **Included**: IAM users, roles, and policies; DynamoDB table metadata and items; SQS queues and in-flight/persisted
  messages; SNS topics and subscriptions; SSM parameters and Secrets Manager secrets; S3 bucket metadata, object metadata,
  noncurrent versions, and pending multipart upload parts; and all referenced S3 payload blob files.
- **Excluded**: External container engine state (such as live Docker containers and named volumes for RDS database
  engines), in-flight Lambda microVM filesystems, ephemeral runtime caches, and gateway metrics.
- **Secrets and privacy**: Exported records include stored secrets, parameters, and credentials in plaintext form within
  the archive's records payload. Archives are created with restrictive permissions (`0600`). Never commit or distribute
  archives containing sensitive keys, passwords, or production credentials.

## Exporting persistent state

State export operates directly on an offline persistent storage directory. TinyStack must be stopped before
exporting so SQLite can acquire an exclusive lock and ensure consistent snapshot integrity.

```bash
tinystack state export --storage-dir ~/.tinystack --output ./fixtures/dev-state.tar.gz
```

### Options

| Flag | Description | Default |
| --- | --- | --- |
| `--storage-dir <dir>` | Path to the TinyStack persistent storage directory containing `state.db` | Required |
| `--output <file>` | Path for the generated archive (`.tar.gz` or `.tar`) | Required |
| `--overwrite` | Overwrite the destination file if it already exists | `false` |

### Safety and offline requirements

- **Offline store required**: Export directly inspects the underlying SQLite database using exclusive locking. If an
  active TinyStack process holds a lock on the database, export halts immediately with an error rather than exporting
  a partial or tearing state.
- **Source preservation**: If the storage directory or `state.db` does not exist, export fails explicitly rather than
  creating empty directories or databases.
- **Atomic publication**: Export writes to a private temporary file in the destination directory and atomically
  publishes the file (using link-no-replace by default or rename when `--overwrite` is specified) once all records and
  blobs are verified. Any failure or cancellation cleans up the temporary file without corrupting or replacing existing files.
- **Overlap protection**: Export refuses output paths that collide with `state.db`, SQLite journal/WAL/SHM sidecars,
  the storage directory itself, or the `blobs/` directory (including symlinked paths).

## Importing persistent state

State import restores a state fixture archive into a brand new, clean persistent storage directory. Import operates
completely offline and creates a fully functional storage layout containing `state.db` (initialized to the current
schema version) and all referenced S3 blobs.

```bash
tinystack state import --input ./fixtures/dev-state.tar.gz --storage-dir ~/.tinystack-restored
```

### Options

| Flag | Description | Default |
| --- | --- | --- |
| `--input <file>` | Path to the fixture archive to import (`.tar.gz` or `.tar`) | Required |
| `--storage-dir <dir>` | Target storage directory path (must not already exist) | Required |

### Conservative fresh-destination semantics

To guarantee that active databases, existing volumes, and accidental directory targets are never overwritten or
clobbered, `tinystack state import` strictly enforces fresh-destination semantics:

- **Destination must not exist**: The target directory specified by `--storage-dir` must **not** already exist, even
  if it is an empty directory, a regular file, or a symlink.
- **No overwrite or merge**: There is no `--overwrite` or merge flag. If the target path exists, import aborts
  immediately and leaves all existing data completely untouched.
- **Parent directory must exist**: The parent directory of `--storage-dir` must already exist and be a directory.
- **Hazard detection**: The target directory and input archive cannot collide, alias, or nest within one another.

### Staging and atomic publication

Import uses transactional, staged extraction with atomic publication on supported platforms:

1. **Private temporary staging**: Import creates a private `0700` temporary directory as a sibling to the destination
   on the same filesystem (e.g. `.tinystack-import-*`).
2. **Single-pass streaming validation**: The input archive is read and validated in a single pass. Manifest format,
   canonical checksums, lexical key ordering, blob closures, and tar/gzip trailers are verified. Blobs are streamed
   directly into `staging/blobs/` with private `0600` permissions without buffering full payloads into memory.
3. **SQLite schema population**: The SQLite database (`state.db`) is initialized directly in staging using the latest
   schema migrations. Committed records are written in a single SQLite transaction and the database connection is closed cleanly.
4. **File and directory synchronization**: Before publication, all written blob files and the SQLite database are flushed
   to disk. Where supported by the operating system and filesystem (POSIX platforms), directory entries are synchronized via
   directory file descriptor `fsync`. On Windows, directory handle flushing is avoided because `FlushFileBuffers` returns
   an access error on directory handles.
5. **Atomic no-replace publication**: The staging directory is atomically renamed to the final destination without
   replacement (`RENAME_EXCL` on macOS Darwin, `RENAME_NOREPLACE` on Linux, `MoveFileEx` without replace on Windows).
   Concurrent directory creations safely fail the operation without modifying or replacing the target. On unsupported platforms
   or filesystems lacking atomic no-replace publication, import returns an actionable error rather than risking accidental replacement.
6. **Clean failure cleanup**: Any validation failure, I/O error, or process cancellation removes the temporary staging
   directory completely, leaving the destination untouched.

## Archive format

State archives package data into a standard tarball (`.tar.gz` or `.tar`) containing three primary components in
strict sequential order:

1. `manifest.json`: Root metadata containing format version `1`, TinyStack version, record count, totals, and
   canonical SHA-256 digests for records and all referenced blobs.
2. `records.json`: A deterministic array of all committed service records sorted strictly by byte-ordered key.
   Both `key` and `value` are encoded as base64 byte slices to guarantee 100% fidelity for binary keys and payloads.
3. `blobs/<id>`: Deduplicated binary payloads for referenced S3 objects, versions, and multipart upload parts.
   Blob IDs are immutable random identifiers assigned by TinyStack's storage layer (not content hashes); each blob's
   SHA-256 integrity checksum is declared in the manifest.

### Reproducibility and integrity

- **Deterministic output**: Tar headers use fixed Unix epoch timestamps (`1970-01-01T00:00:00Z`), private file
  permissions (`0600`), and zeroed IDs. Identical store states produce bit-for-bit identical archives.
- **Deduplicated S3 blobs**: Objects, versions, and copy operations sharing underlying blob IDs are referenced
  by ID and included only once in the archive.
- **Bounded memory streaming**: Blobs are streamed directly between disk and archive using bounded 64 KiB buffers rather than
  buffering entire payloads in memory.
- **Integrity verification**: All records and blobs are validated against SHA-256 checksums to detect corruption,
  truncation, or tampering.

## Compatibility and limits

State fixture archives are governed by defined format specifications and operational limits:

- **Format version**: Format version `1`. Archives use standard tar packaging, optionally wrapped in a single gzip stream
  (`.tar.gz` or `.tar`), autodetected by magic bytes. Readers reject multi-member gzip streams, trailing junk data, and
  unsupported archive formats.
- **Record limits**:
  - Maximum records: 1,000,000 records.
  - Maximum key length: 4,096 bytes (4 KiB).
  - Maximum value size: 64 MiB.
  - Maximum serialized records payload: 1 GiB.
- **Blob limits**:
  - Maximum blobs: 1,000,000 blobs.
  - Maximum individual blob size: 5 GiB.
  - Maximum manifest size: 32 MiB.
  - Streaming buffer size: bounded 64 KiB chunks.
- **Platform requirements**:
  - Atomic no-replace publication requires filesystem rename support without replacement (`RenamexNp` on macOS Darwin,
    `Renameat2` on Linux, and native `MoveFileEx` without replace on Windows).
  - Synchronous durability operations adapt to OS capabilities: POSIX platforms synchronize directory descriptors, while
    Windows flushes individual files.
- **Scope caveats**:
  - Fixtures restore committed SQLite service records and disk-backed S3 object payloads. They do not reconstruct
    or replay active in-memory session states, background worker queues, runtime log buffers, container volumes (such
    as RDS container engines), or configuration credentials files.

## Reusable integration testing workflow

State fixtures enable rapid integration testing without re-running long provisioning scripts for each test run:

```bash
# Configure local AWS CLI environment credentials
export AWS_ACCESS_KEY_ID=test
export AWS_SECRET_ACCESS_KEY=test
export AWS_DEFAULT_REGION=us-east-1

# Create an isolated temporary workspace with unique paths
WORK_DIR=$(mktemp -d /tmp/tinystack-fixture.XXXXXX)
SEED_STORE="$WORK_DIR/seed-store"
FIXTURES_DIR="$WORK_DIR/test-fixtures"
mkdir -p "$FIXTURES_DIR"
ARCHIVE_PATH="$FIXTURES_DIR/seed.tar.gz"

CI_PARENT_DIR="$WORK_DIR/test-run-1"
mkdir -p "$CI_PARENT_DIR"
RESTORED_STORE="$CI_PARENT_DIR/store"

# 1. Provision baseline services in development using persistent storage
cat <<EOF > "$WORK_DIR/tinystack-dev.yaml"
storage:
  mode: persistent
  path: $SEED_STORE
EOF

tinystack start --config "$WORK_DIR/tinystack-dev.yaml" &
TINYSTACK_PID=$!

# Wait until TinyStack is healthy and accepting requests
until curl -s http://127.0.0.1:4566/_tinystack/health | grep -q '"status":"ok"'; do
  sleep 0.1
done

# Create baseline test fixture resources (e.g. S3 bucket and sample object)
aws --endpoint-url http://localhost:4566 s3 mb s3://app-assets
echo "Hello from seed fixture" > "$WORK_DIR/sample.txt"
aws --endpoint-url http://localhost:4566 s3 cp "$WORK_DIR/sample.txt" s3://app-assets/sample.txt

# Gracefully shut down to flush WAL and release database locks
kill -TERM $TINYSTACK_PID
wait $TINYSTACK_PID 2>/dev/null

# 2. Export baseline fixture offline (parent directory $FIXTURES_DIR must already exist)
tinystack state export --storage-dir "$SEED_STORE" --output "$ARCHIVE_PATH"

# 3. In CI test runs, import into a fresh nonexistent target directory
# Note: $CI_PARENT_DIR must exist, but $RESTORED_STORE must NOT exist prior to import.
tinystack state import --input "$ARCHIVE_PATH" --storage-dir "$RESTORED_STORE"

# 4. Start TinyStack against the restored persistent store in CI
cat <<EOF > "$WORK_DIR/tinystack-ci.yaml"
storage:
  mode: persistent
  path: $RESTORED_STORE
EOF

tinystack start --config "$WORK_DIR/tinystack-ci.yaml" &
TINYSTACK_PID=$!

until curl -s http://127.0.0.1:4566/_tinystack/health | grep -q '"status":"ok"'; do
  sleep 0.1
done

# Verify restored state by downloading and comparing seeded object
aws --endpoint-url http://localhost:4566 s3 cp s3://app-assets/sample.txt "$WORK_DIR/restored-sample.txt"
cmp "$WORK_DIR/sample.txt" "$WORK_DIR/restored-sample.txt"

# Gracefully shut down restored instance and clean up workspace
kill -TERM $TINYSTACK_PID
wait $TINYSTACK_PID 2>/dev/null
rm -rf "$WORK_DIR"
```
