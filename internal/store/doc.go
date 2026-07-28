// Package store is dorang's persistence layer: one logical schema, two
// dialects, and one Go API that never asks the caller which one it got.
//
// # One schema, two dialects
//
// SQLite is the notebook default and must work with zero external software
// (DESIGN 0.2); PostgreSQL is the team and enterprise path. Both are driven by
// the same *Store and the same methods. Dialect differences live behind an
// unexported strategy: placeholder syntax, DDL text, the migration lock, and
// partitioning. The one place a caller may legitimately care -- retention by
// DROP PARTITION versus retention by DELETE -- is reported rather than hidden,
// through Store.Partitioning and MaintenanceReport.Mechanism. Everything else
// is identical, including the SQL of every query in the ledger query set.
//
// # Encodings
//
// Timestamps are int64 unix microseconds in UTC in every column and every
// parameter. SQLite has no timestamp type, so one dialect had to give way; an
// integer is exact, sorts correctly, carries no time zone, and turns daily
// partition bounds into arithmetic. Use Micros and TimeAt at the boundary.
//
// Money is int64 nano-units. DESIGN 8.3 rejects nano-units for pricing
// arithmetic -- a sub-nano per-token rate rounds to zero -- and that objection
// is about computation, which happens in exact decimal with 128-bit
// intermediates and rounds exactly once, before anything reaches this package.
// What is stored is the already-rounded settled amount. Writers range-check
// against MaxAmountNano so an overflow is ErrAmountRange and never a negative
// cost.
//
// # Migrations
//
// Migrations are embedded with go:embed, ordered by numeric prefix, applied
// inside one transaction, recorded in schema_migrations with a checksum, and
// idempotent. Two nodes may start at once: PostgreSQL takes a transaction-scoped
// advisory lock, SQLite opens the transaction with BEGIN IMMEDIATE. Editing an
// already-applied migration is refused rather than silently ignored.
//
// # The ledger is bounded by construction
//
// Every ledger query requires a bounded, ordered time range and paginates by
// keyset on (ts, id). An unbounded range is ErrUnboundedRange, and a range
// wider than Config.MaxTimeRange is ErrRangeTooWide. Indexes exist for exactly
// the six queries in DESIGN 9.3 and for nothing else.
//
// # Partitions ship with the writer
//
// On PostgreSQL request_logs, request_log_tags and request_traces are
// range-partitioned by day. EnsurePartitions pre-creates at least two days
// ahead, and the writer additionally recovers from a missing partition in-line,
// so the first midnight after metering ships is uneventful (DESIGN 9.5, R1-15).
//
// # What this package does not do
//
// It does not sit on the hot path. Authorization runs off an in-memory snapshot
// and metering is asynchronous (DESIGN 9.1); the methods here serve the snapshot
// build, the metering flush, and the administrative surface.
package store
