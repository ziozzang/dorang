package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/admin"
	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/meter"
	"github.com/ziozzang/dorang/internal/server"
)

// ---------------------------------------------------------------------------
// The unfiltered /spend/logs refusal
// ---------------------------------------------------------------------------

// globalAdmin is an administrative caller with nothing else attached. The
// refusal under test happens after authorization, so the identity only has to
// be admissible.
type globalAdmin struct{}

func (globalAdmin) ActorKind() string       { return "master" }
func (globalAdmin) ActorID() string         { return "" }
func (globalAdmin) IsAdmin() bool           { return true }
func (globalAdmin) AdminScope() admin.Scope { return admin.GlobalScope() }

type globalAdminAuth struct{}

func (globalAdminAuth) AuthenticateHeader(context.Context, http.Header) (admin.Principal, error) {
	return globalAdmin{}, nil
}

// TestUnfilteredSpendLogsIsNotImplementedRatherThanInternalError drives the real
// ledger adapter through the real administration surface and asserts the STATUS
// and the MESSAGE an operator receives.
//
// The defect it pins is not "the wrong error was returned". It is that the
// sentinel was pasted into the message as text —
//
//	errors.New("app: " + msg + " (" + admin.ErrUnsupported.Error() + ")")
//
// — so errors.Is could not see it, admin.faultFor's 501 case never matched, and
// the request fell through to the 500 default two lines below. Reading a log
// would not have shown it: the text was identical either way. The whole
// observable difference is on the wire, which is why this asserts there.
//
// A 500 tells a client "gateway fault, retry". The truth is "add a query
// parameter", and the message that says WHICH parameters work was discarded
// along with the status.
func TestUnfilteredSpendLogsIsNotImplementedRatherThanInternalError(t *testing.T) {
	st, _ := openTestStore(t, nil)
	api, err := admin.New(admin.Config{
		Auth:   globalAdminAuth{},
		Ledger: &adminLedger{st: st},
	})
	if err != nil {
		t.Fatalf("admin.New: %v", err)
	}

	const window = "start_date=2026-07-01&end_date=2026-07-02"
	for _, c := range []struct {
		name, query string
		// want is a phrase the message must carry: the point of a 501 here is
		// that it names a way forward.
		want string
	}{
		{"no filter at all", window, "key_id"},
		{"user_id, which has no index", window + "&user_id=u-1", "per-user index"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			api.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/spend/logs?"+c.query, nil))

			if w.Code != http.StatusNotImplemented {
				t.Fatalf("status = %d, want 501; body = %s", w.Code, w.Body.String())
			}
			var body struct {
				Error struct {
					Message string `json:"message"`
					Code    string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decoding %s: %v", w.Body.String(), err)
			}
			if body.Error.Code == "internal_error" {
				t.Errorf("code = %q: the refusal was reclassified as a gateway fault",
					body.Error.Code)
			}
			if !strings.Contains(body.Error.Message, c.want) {
				t.Errorf("message = %q, want it to contain %q — a caller who is not "+
					"told which filters work retries the same query",
					body.Error.Message, c.want)
			}
		})
	}
}

// TestLedgerRefusalsCarryTheUnsupportedSentinel is the same defect at the seam,
// where it is cheapest to see. internal/admin/ui.go makes its own
// errors.Is(err, ErrUnsupported) check, so the operator UI misclassified this
// refusal too.
func TestLedgerRefusalsCarryTheUnsupportedSentinel(t *testing.T) {
	st, _ := openTestStore(t, nil)
	l := &adminLedger{st: st}
	rng := admin.Range{
		Start: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC),
	}

	_, err := l.ListRequests(context.Background(), admin.LogQuery{Range: rng})
	if err == nil {
		t.Fatal("an unfiltered ledger query was accepted")
	}
	if !errors.Is(err, admin.ErrUnsupported) {
		t.Errorf("ListRequests error %v does not satisfy errors.Is(err, admin.ErrUnsupported)", err)
	}
	if _, err := l.Report(context.Background(), admin.ReportQuery{}); err == nil {
		t.Fatal("Report was accepted")
	} else if !errors.Is(err, admin.ErrUnsupported) {
		t.Errorf("Report error %v does not satisfy errors.Is(err, admin.ErrUnsupported)", err)
	}
}

// ---------------------------------------------------------------------------
// deployment_id and streamed
// ---------------------------------------------------------------------------

// traceSink collects what the meter flushes.
type traceSink struct {
	mu     sync.Mutex
	traces []meter.Trace
}

func (s *traceSink) WriteRollups(context.Context, []meter.Bucket) error { return nil }

func (s *traceSink) WriteTraces(_ context.Context, ts []meter.Trace) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.traces = append(s.traces, ts...)
	return nil
}

func (s *traceSink) rows() []meter.Trace {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]meter.Trace(nil), s.traces...)
}

// TestMeterAdapterCarriesDeploymentAndStreamed pins the two ledger columns that
// had a writer, a reader, a JSON name and no producer at all.
//
// It reads the values off the SAME fields the response is built from —
// server.Result.Deployment is what x-dorang-deployment is stamped from, and
// server.Event.Streamed is what the response writer computed from the
// Content-Type it actually sent — so the row cannot drift from the answer.
// cmd/dorang's round-trip tests assert the same two values end to end against a
// real response and a real ledger row; this is where a regression is cheapest
// to see.
func TestMeterAdapterCarriesDeploymentAndStreamed(t *testing.T) {
	sink := &traceSink{}
	m := meter.New(meter.Config{Sink: sink, SpoolDir: t.TempDir()})
	t.Cleanup(func() { _ = m.Close() })

	a := &meterAdapter{m: m, now: time.Now}
	a.Record(server.Event{
		RequestID: "req-1",
		KeyID:     "key-1",
		Model:     "model-x",
		Status:    http.StatusOK,
		Streamed:  true,
		Result:    server.Result{Deployment: "dep-7|prov-1|upstream-x"},
	})
	if err := m.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	rows := sink.rows()
	if len(rows) != 1 {
		t.Fatalf("%d traces recorded, want 1", len(rows))
	}
	got := rows[0]
	if got.DeploymentID != "dep-7|prov-1|upstream-x" {
		t.Errorf("trace deployment_id = %q, want the value x-dorang-deployment carries; "+
			"per-deployment attribution was kept in the header and lost in the ledger",
			got.DeploymentID)
	}
	if !got.Streamed {
		t.Error("trace streamed = false for a request the server answered with an event stream")
	}
}

// ---------------------------------------------------------------------------
// server.max_body_bytes
// ---------------------------------------------------------------------------

// TestConfigBodyCapDefaultMatchesTheServers pins the one duplicated constant.
//
// internal/config cannot import internal/server — it is the schema, and a
// schema that needs the HTTP surface cannot be loaded by a tool that does not
// build one — so the 32 MiB default is spelled in both. This package imports
// both, and is therefore the only place the two can be compared.
func TestConfigBodyCapDefaultMatchesTheServers(t *testing.T) {
	var c config.Config
	c.ApplyDefaults()
	if got := c.Server.MaxBodyBytes.Bytes(); got != server.DefaultMaxBodyBytes {
		t.Errorf("config default body cap = %d, server.DefaultMaxBodyBytes = %d: "+
			"a deployment that sets nothing would silently change its own limit",
			got, server.DefaultMaxBodyBytes)
	}
}
