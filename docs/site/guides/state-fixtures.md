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
- **Bounded memory streaming**: Blobs are streamed directly to the archive using bounded 64 KiB buffers rather than
  buffering entire payloads in memory.
- **Integrity verification**: All records and blobs are validated against SHA-256 checksums to detect corruption,
  truncation, or tampering.

## State import

State import (`tinystack state import`) is currently in development and will restore state archives directly into clean
persistent storage directories for local development and test automation.
