package store

import (
	"database/sql"
	"encoding/json"
)

// List-valued columns are JSON text in both dialects. They are configuration,
// read once into the in-memory authorization snapshot (DESIGN 9.1), never
// filtered on in SQL -- so a native array type would buy nothing and cost the
// one-schema property. Request-log tags are the exception that proves the
// rule: they ARE filtered on, so they are a normalized table instead
// (DESIGN 9.3).

func encodeStrings(v []string) string {
	if len(v) == 0 {
		return "[]"
	}
	b, err := json.Marshal(v)
	if err != nil {
		// []string cannot fail to marshal.
		return "[]"
	}
	return string(b)
}

func decodeStrings(s string) []string {
	if s == "" || s == "[]" || s == "null" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		// A value that is not a JSON array is not a list of anything. Treat it
		// as empty rather than fail the read: an unreadable allow-list must
		// deny, and an empty allow-list is the denying value everywhere it is
		// consulted.
		return nil
	}
	return out
}

// nullableInt converts a scanned nullable integer into the pointer form the
// structs use, where nil means "not configured" and 0 means "zero".
func nullableInt(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}

// ptrInt is the inverse, for binding.
func ptrInt(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func int64p(v int64) *int64 { return &v }
