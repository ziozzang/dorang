package config

import (
	"os"
	"testing"
)

// TestMain sets the one environment variable the shared fixtures reference.
//
// The fixtures used to spell their secret `key_ref`, because a vault reference
// was the one source whose loading touched neither the environment nor the
// filesystem. That is exactly why key_ref is now refused — it touched nothing
// because it resolved to nothing — so the fixtures name a variable instead, and
// this is where it is set.
func TestMain(m *testing.M) {
	os.Setenv("DORANG_TEST_FIXTURE_KEY", "fixture-secret")
	os.Exit(m.Run())
}
