package store

import "errors"

// Sentinel errors. Callers match with errors.Is.
var (
	// ErrNotFound is returned when a lookup by primary key finds nothing.
	ErrNotFound = errors.New("store: not found")

	// ErrExists is returned when an insert would collide with an existing
	// primary key. It is separate from a driver's own constraint error so that
	// a caller can tell "this id is taken" from "the database is unreachable"
	// without matching on message text.
	ErrExists = errors.New("store: already exists")

	// ErrUnboundedRange is returned when a ledger query is asked for an
	// unbounded or improperly ordered time range. DESIGN 9.3: unbounded
	// search is refused, not answered slowly.
	ErrUnboundedRange = errors.New("store: ledger query requires a bounded time range")

	// ErrRangeTooWide is returned when a ledger query's time range exceeds
	// Config.MaxTimeRange. A ten-year range is unbounded in every way that
	// costs anything.
	ErrRangeTooWide = errors.New("store: ledger time range exceeds the configured maximum")

	// ErrAmountRange is returned when a monetary amount does not fit the
	// nano-unit representation with aggregation headroom (DESIGN 8.3:
	// overflow is an error, not a negative cost).
	ErrAmountRange = errors.New("store: monetary amount out of range")

	// ErrNoPartitioning is returned by partition-specific operations on a
	// dialect that has no partitions. It exists so that a caller who needs to
	// know cannot be fooled by a silent no-op.
	ErrNoPartitioning = errors.New("store: dialect does not support partitioning")

	// ErrLegacyDisabled is returned when a credential verifies only under
	// legacy_sha256 and legacy verification is off or past its sunset date
	// (DESIGN 2.4).
	ErrLegacyDisabled = errors.New("store: legacy_sha256 verification is disabled")

	// ErrBadCredential is returned when a token does not verify. It is
	// deliberately indistinguishable from ErrNotFound at the API boundary;
	// callers should not tell the two apart to their own callers.
	ErrBadCredential = errors.New("store: credential does not verify")

	// ErrNoPepper is returned when a dorang_v1 operation is attempted without
	// a configured pepper. A pepperless "HMAC" is an unsalted digest wearing a
	// costume.
	ErrNoPepper = errors.New("store: dorang_v1 requires a key pepper")

	// ErrDirtySchema is returned when an already-applied migration's checksum
	// no longer matches the embedded file.
	ErrDirtySchema = errors.New("store: applied migration has been modified")

	// ErrBadIdentifier is returned when a caller-supplied SQL identifier (an
	// import source table, say) is not a plain identifier.
	ErrBadIdentifier = errors.New("store: invalid SQL identifier")
)
