// Package tabular encodes table rows without depending on Glue or S3.
//
// Column types use Hive syntax. A row contains one string pointer per column;
// nil is SQL NULL, dates use YYYY-MM-DD, timestamps use a space-separated UTC
// date/time, binary values contain raw bytes, and nested values contain JSON.
// Parquet decimal conversion preserves up to 38 digits without floating point.
//
// Format.Records preserves the historical Athena text, CSV and JSON reader
// behavior. Format.Write writes these formats and Parquet; text outputs support
// GZIP or NONE and Parquet supports GZIP, SNAPPY, ZSTD and UNCOMPRESSED pages.
// Infer reads a Parquet footer. StreamParquet accepts a range-capable ReaderAt,
// projects the requested top-level columns, and delivers rows incrementally.
// Athena currently fetches whole objects and loads all referenced table columns
// for its SQLite engine; its scanned-byte statistic counts those actual bytes.
package tabular
