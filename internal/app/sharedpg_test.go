package app

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/config"
)

// The real App assembly, storage and HTTP streaming path must use the shared
// broker, in addition to the three-process tests of the semaphore itself.
func TestSharedPGGatewayKeepsAllSixAccountSlots(t *testing.T) {
	dsn := os.Getenv("DORANG_TEST_PG")
	if dsn == "" {
		t.Skip("DORANG_TEST_PG is not set")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal("open test database")
	}
	schema := "account_app_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if _, err = admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec("DROP SCHEMA " + schema + " CASCADE"); admin.Close() })
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("invalid test database URL")
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	t.Setenv("DORANG_ACCOUNT_APP_PG", u.String())
	t.Setenv("DORANG_ACCOUNT_APP_NODE", "account-app")
	tenRequestsShareAccounts(t, true, func(c *config.Config) {
		c.Storage.Driver = "postgres"
		c.Storage.Postgres.URLEnv = "DORANG_ACCOUNT_APP_PG"
		c.Cluster.Enabled = true
		c.Cluster.CapacityMode = config.CapacityModeSharedPG
		c.Cluster.NodeIDEnv = "DORANG_ACCOUNT_APP_NODE"
	})
	if err := admin.PingContext(context.Background()); err != nil {
		t.Fatal(err)
	}
}
