package capacity

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func accountTestDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dsn := os.Getenv("DORANG_TEST_PG")
	if dsn == "" {
		t.Skip("DORANG_TEST_PG is not set")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "account_test_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if _, err = admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal("create isolated schema", err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("test DSN invalid")
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE capacity_leases(id text primary key,node_id text not null,axis text not null,axis_key text not null,amount bigint not null,used bigint not null default 0,acquired_at bigint not null,expires_at bigint not null)`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(); admin.Exec("DROP SCHEMA " + schema + " CASCADE"); admin.Close() })
	return db, u.String()
}

func accountBroker(db *sql.DB, node string) *Broker {
	pg := NewPGAccounts(db, node)
	if os.Getenv("DORANG_ACCOUNT_FAST_LEASE") == "1" {
		pg.ttl = time.Second
	}
	b := New(Config{SharedAccounts: pg, CredentialGroups: map[string]int{"one": 3, "two": 3}, Queues: QueueConfig{CredentialGroups: map[string]int{"one": 20, "two": 20}, MaxWait: map[string]time.Duration{"default": 3 * time.Second}}})
	b.SetShare(30) // Warm nodes must never divide these account slots.
	return b
}

func accountsRequest() Request {
	return Request{PrincipalID: "test", Candidates: []Candidate{{ID: "one", CapacityGroup: "one"}, {ID: "two", CapacityGroup: "two"}}, OnCapacity: Spill}
}

func TestPGAccountNodeProcess(t *testing.T) {
	mode := os.Getenv("DORANG_ACCOUNT_HELPER")
	if mode == "" {
		t.Skip("subprocess helper")
	}
	db, err := sql.Open("pgx", os.Getenv("DORANG_TEST_PG"))
	if err != nil {
		t.Fatal("open helper database")
	}
	defer db.Close()
	b := accountBroker(db, os.Getenv("DORANG_ACCOUNT_NODE"))
	defer b.Close()
	if mode == "hold" {
		res, err := b.Acquire(context.Background(), Request{Candidates: []Candidate{{ID: "one", CapacityGroup: "one"}}})
		if err != nil {
			t.Fatal(err)
		}
		defer res.Release()
		fmt.Println("READY")
		select {}
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := b.Acquire(context.Background(), accountsRequest())
			if err != nil {
				t.Error(err)
				return
			}
			defer res.Release()
			ctx, cancel := res.Context(context.Background())
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, "GET", os.Getenv("DORANG_ACCOUNT_URL")+"/"+res.CredentialID(), nil)
			rsp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error("fake upstream call", err)
				return
			}
			io.Copy(io.Discard, rsp.Body)
			rsp.Body.Close()
			if rsp.StatusCode != 200 {
				t.Error("fake upstream status", rsp.StatusCode)
			}
		}()
	}
	wg.Wait()
}

func accountChild(t *testing.T, dsn, mode, node, endpoint string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestPGAccountNodeProcess$", "-test.timeout=30s")
	cmd.Env = append(os.Environ(), "DORANG_TEST_PG="+dsn, "DORANG_ACCOUNT_HELPER="+mode, "DORANG_ACCOUNT_NODE="+node, "DORANG_ACCOUNT_URL="+endpoint, "DORANG_ACCOUNT_FAST_LEASE=1")
	t.Cleanup(func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
	})
	return cmd
}

func TestPGThreeNodesUseSixSlotsWithoutExceedingEitherAccount(t *testing.T) {
	db, dsn := accountTestDB(t)
	var active, peak [2]atomic.Int64
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := 0
		if strings.Contains(r.URL.Path, "two") {
			i = 1
		}
		n := active[i].Add(1)
		defer active[i].Add(-1)
		for old := peak[i].Load(); n > old; old = peak[i].Load() {
			if peak[i].CompareAndSwap(old, n) {
				break
			}
		}
		<-release
		time.Sleep(10 * time.Millisecond)
		w.WriteHeader(200)
	}))
	t.Cleanup(up.Close)
	t.Cleanup(unblock)
	children := make([]*exec.Cmd, 3)
	for i := range children {
		children[i] = accountChild(t, dsn, "requests", fmt.Sprint(i), up.URL)
		if err := children[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && (active[0].Load() != 3 || active[1].Load() != 3) {
		time.Sleep(5 * time.Millisecond)
	}
	if active[0].Load() != 3 || active[1].Load() != 3 {
		t.Fatalf("three nodes did not fill both accounts: %d,%d", active[0].Load(), active[1].Load())
	}
	var slots int
	if err := db.QueryRow("SELECT count(*) FROM capacity_leases WHERE axis='account'").Scan(&slots); err != nil || slots != 6 {
		t.Fatalf("shared rows=%d, err=%v", slots, err)
	}
	unblock()
	for _, cmd := range children {
		if err := cmd.Wait(); err != nil {
			t.Fatal("node process failed", err)
		}
	}
	for i := range peak {
		if peak[i].Load() != 3 {
			t.Errorf("account %d peak=%d, want 3", i, peak[i].Load())
		}
	}
	if err := db.QueryRow("SELECT count(*) FROM capacity_leases WHERE axis='account'").Scan(&slots); err != nil || slots != 0 {
		t.Fatalf("released rows=%d err=%v", slots, err)
	}
}

func TestPGKilledNodeReturnsItsSlot(t *testing.T) {
	db, dsn := accountTestDB(t)
	b := accountBroker(db, "survivor")
	defer b.Close()
	req := Request{Candidates: []Candidate{{ID: "one", CapacityGroup: "one"}}}
	var held []*Reservation
	for i := 0; i < 2; i++ {
		res, err := b.Acquire(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, res)
		defer res.Release()
	}
	cmd := accountChild(t, dsn, "hold", "crashed", "")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(out)
		for scanner.Scan() {
			if scanner.Text() == "READY" {
				ready <- true
				return
			}
		}
		ready <- false
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("child did not acquire slot")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("child readiness timeout")
	}
	if res, ok := b.TryAcquire(req); ok {
		res.Release()
		t.Fatal("fourth concurrent account request was admitted")
	}
	var expires int64
	if err = db.QueryRow("SELECT expires_at FROM capacity_leases WHERE node_id='crashed'").Scan(&expires); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if res, ok := b.TryAcquire(req); ok {
			var now int64
			err = db.QueryRow("SELECT (extract(epoch from clock_timestamp())*1000000)::bigint").Scan(&now)
			res.Release()
			if err != nil || now < expires {
				t.Fatalf("dead owner's unexpired lease was recycled: now=%d expires=%d err=%v", now, expires, err)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("killed node's shared account slot was not reclaimed")
}

func TestPGIdleLeaseExpiresAndLostConnectionCancelsRequest(t *testing.T) {
	db, _ := accountTestDB(t)
	p := NewPGAccounts(db, "paused")
	p.ttl = 300 * time.Millisecond
	// Hold the renewal loop by replacing it with a raw session carrying the same
	// idle timeout. PostgreSQL, rather than a healthy peer's clock, must free it.
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer discardAccountConn(conn)
	key := accountLockKey("one", 0)
	var ok bool
	if err = conn.QueryRowContext(context.Background(), "SELECT pg_try_advisory_lock($1)", key).Scan(&ok); err != nil || !ok {
		t.Fatal(err)
	}
	if _, err = conn.ExecContext(context.Background(), "SET idle_session_timeout='300ms'"); err != nil {
		t.Fatal(err)
	}
	// Other two slots can be taken; the third must wait for the idle owner.
	l1, err := p.try("one", 3)
	if err != nil || l1 == nil {
		t.Fatal(err)
	}
	defer l1.release()
	l2, err := p.try("one", 3)
	if err != nil || l2 == nil {
		t.Fatal(err)
	}
	defer l2.release()
	if l, err := p.try("one", 3); err != nil || l != nil {
		if l != nil {
			l.release()
		}
		t.Fatal("idle owner slot was reused before expiry")
	}
	time.Sleep(450 * time.Millisecond)
	l3, err := p.try("one", 3)
	if err != nil || l3 == nil {
		t.Fatal("expired idle session was not reclaimed", err)
	}
	defer l3.release()
	res := &Reservation{account: l3}
	ctx, cancel := res.Context(context.Background())
	defer cancel()
	var pid int
	if err = l3.conn.QueryRowContext(context.Background(), "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("SELECT pg_terminate_backend($1)", pid); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("lost account slot did not cancel generation")
	}
}

func TestPGDatabaseFailureRefusesAndBoundsWaiting(t *testing.T) {
	db, _ := accountTestDB(t)
	b := New(Config{SharedAccounts: NewPGAccounts(db, "offline"), CredentialGroups: map[string]int{"one": 3}, Queues: QueueConfig{CredentialGroups: map[string]int{"one": 1}, MaxWait: map[string]time.Duration{"default": 50 * time.Millisecond}}})
	defer b.Close()
	db.Close()
	req := Request{Candidates: []Candidate{{ID: "one", CapacityGroup: "one"}}}
	if res, ok := b.TryAcquire(req); ok {
		res.Release()
		t.Fatal("request was admitted without shared database")
	}
	started := time.Now()
	if res, err := b.Acquire(context.Background(), req); err == nil {
		res.Release()
		t.Fatal("offline database granted a slot")
	}
	if time.Since(started) > time.Second || b.Snapshot().Waiting != 0 || b.Snapshot().Reservations != 0 {
		t.Fatal("offline database left a waiter or reservation")
	}
}
