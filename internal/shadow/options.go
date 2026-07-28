package shadow

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Mode is the shadow mode of DESIGN §14.1.
type Mode uint8

const (
	// ModeOff shadows nothing. A [Shadower] built with it is a no-op that still
	// answers Stats, so a caller does not need a nil check.
	ModeOff Mode = iota
	// ModeMirror serves from dorang, sends the same request to the reference
	// asynchronously, and records only the reference's result.
	ModeMirror
	// ModeCompare is ModeMirror plus the structural diff.
	ModeCompare
)

// String names the mode as the configuration file spells it.
func (m Mode) String() string {
	switch m {
	case ModeMirror:
		return "mirror"
	case ModeCompare:
		return "compare"
	default:
		return "off"
	}
}

// ParseMode reads a mode from its configuration spelling.
func ParseMode(s string) (Mode, error) {
	switch s {
	case "", "off":
		return ModeOff, nil
	case "mirror":
		return ModeMirror, nil
	case "compare":
		return ModeCompare, nil
	}
	return ModeOff, fmt.Errorf("shadow: unknown mode %q: want off, mirror or compare", s)
}

// Options configures a [Shadower].
//
// It is plain Go types on purpose: internal/config translates the file into
// this, the same way it does for every other leaf package, so that this one can
// be built and tested without a configuration file existing.
type Options struct {
	// Mode selects off, mirror or compare.
	Mode Mode

	// ReferenceURL is the reference gateway's base URL. The request path is
	// appended to it, so a base with a path prefix works.
	ReferenceURL string
	// ReferenceKey is the reference gateway's own credential, already resolved
	// from api_key_env. The client's credential is never sent there.
	ReferenceKey string
	// ReferenceTimeout bounds one reference call; 0 uses
	// [DefaultReferenceTimeout]. A reference that hangs must give its worker
	// back rather than hold it for the life of the process.
	ReferenceTimeout time.Duration

	// SampleRate is the fraction of requests shadowed, in [0,1].
	SampleRate float64

	// Structural enables the structural diff. With Mode == ModeCompare and
	// Structural false, nothing is compared and [New] refuses.
	Structural bool
	// IgnoreFields are field paths excluded from the diff on top of
	// [defaultIgnore]. A bare name matches that name at any depth, a path
	// starting with "$." matches exactly, and a trailing "*" matches a prefix.
	IgnoreFields []string

	// MaxCostNanoUSDPerDay is the hard daily ceiling on what the reference
	// calls may cost. Zero means unlimited, which [New] refuses for a mode that
	// spends money — §14.1 requires the ceiling, and a ceiling that defaults to
	// "none" is not one.
	MaxCostNanoUSDPerDay int64
	// UnpricedEstimateNanoUSD is charged for a shadow call whose original
	// dorang could not price; 0 uses [DefaultUnpricedEstimateNanoUSD].
	UnpricedEstimateNanoUSD int64

	// QueueSize bounds the work queue; 0 uses [DefaultQueueSize].
	QueueSize int
	// Workers is the number of concurrent reference calls; 0 uses
	// [DefaultWorkers].
	Workers int
	// MaxCaptureBytes bounds what one queued job copies out of an observation,
	// per side; 0 uses [DefaultMaxCaptureBytes]. Queue × this × 2 is the memory
	// shadowing may hold.
	MaxCaptureBytes int

	// ReportPath is the JSONL report file. Empty writes no file, which is legal
	// for ModeMirror and refused for ModeCompare.
	ReportPath string
	// ReportMaxBytes caps the report; 0 uses [DefaultReportMaxBytes]. Past it
	// records are dropped and counted rather than filling a disk (§9.6 rule 1).
	ReportMaxBytes int64
	// ReportWriter overrides the file, for tests and for a deployment that
	// would rather ship the report somewhere else. It is written from one
	// goroutine at a time.
	ReportWriter io.Writer

	// CloseGrace is how long [Shadower.Close] lets the workers drain before it
	// cancels the calls they are waiting on; 0 uses [DefaultCloseGrace].
	//
	// It is a grace and not a wait because a reference gateway that hangs must
	// not be able to hold up dorang's own shutdown. DESIGN §13 gives in-flight
	// requests a grace period and then exits; a diagnostic gets no more than
	// that.
	CloseGrace time.Duration

	// Client issues reference calls. Nil builds one that does not follow
	// redirects, for the reason [defaultClient] gives.
	Client *http.Client
	// Now overrides the clock, for tests.
	Now func() time.Time
	// Logf receives diagnostics. Never called on the request path.
	Logf func(format string, args ...any)
}

// Defaults for the zero fields of [Options].
const (
	// DefaultReferenceTimeout is shorter than a request timeout on purpose: a
	// shadow call that outlives the request it copies is holding a worker for a
	// comparison nobody is waiting for.
	DefaultReferenceTimeout = 60 * time.Second
	DefaultQueueSize        = 256
	DefaultWorkers          = 4
	// DefaultMaxCaptureBytes bounds one side of one queued comparison.
	DefaultMaxCaptureBytes = 512 << 10
	DefaultReportMaxBytes  = 256 << 20
	// DefaultCloseGrace bounds a shutdown against a hanging reference.
	DefaultCloseGrace = 5 * time.Second
	// DefaultUnpricedEstimateNanoUSD is one cent. Charging zero for a request
	// dorang could not price would make the ceiling unenforceable for exactly
	// the traffic whose cost is unknown.
	DefaultUnpricedEstimateNanoUSD = 10_000_000
)

// ErrOff is returned by [New] for [ModeOff]. It is a sentinel rather than an
// error condition: a caller that wants a working no-op uses [NewOff].
var ErrOff = errors.New("shadow: mode is off")

func (o *Options) setDefaults() {
	if o.ReferenceTimeout <= 0 {
		o.ReferenceTimeout = DefaultReferenceTimeout
	}
	if o.QueueSize <= 0 {
		o.QueueSize = DefaultQueueSize
	}
	if o.Workers <= 0 {
		o.Workers = DefaultWorkers
	}
	if o.MaxCaptureBytes <= 0 {
		o.MaxCaptureBytes = DefaultMaxCaptureBytes
	}
	if o.ReportMaxBytes <= 0 {
		o.ReportMaxBytes = DefaultReportMaxBytes
	}
	if o.UnpricedEstimateNanoUSD <= 0 {
		o.UnpricedEstimateNanoUSD = DefaultUnpricedEstimateNanoUSD
	}
	if o.CloseGrace <= 0 {
		o.CloseGrace = DefaultCloseGrace
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if o.Client == nil {
		o.Client = defaultClient(o.ReferenceTimeout)
	}
}

// validate rejects a configuration that would produce a report nobody should
// believe.
func (o *Options) validate() error {
	if o.SampleRate < 0 || o.SampleRate > 1 {
		return fmt.Errorf("shadow: sample_rate %v is not in [0,1]", o.SampleRate)
	}
	u, err := url.Parse(o.ReferenceURL)
	if err != nil {
		return fmt.Errorf("shadow: reference url %q: %w", o.ReferenceURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("shadow: reference url %q: scheme must be http or https", o.ReferenceURL)
	}
	if u.Host == "" {
		return fmt.Errorf("shadow: reference url %q: no host", o.ReferenceURL)
	}
	// §14.1 requires a daily ceiling because both modes spend money. A ceiling
	// that may be omitted is a ceiling that will be, on the deployment where it
	// matters most.
	if o.MaxCostNanoUSDPerDay <= 0 {
		return errors.New("shadow: max_cost_usd_per_day must be set: " +
			"every sampled request is sent twice and therefore costs twice (§14.1)")
	}
	if o.Mode == ModeCompare && !o.Structural {
		return errors.New("shadow: mode is compare but no comparison is enabled; " +
			"use mode: mirror instead")
	}
	if o.Mode == ModeCompare && o.ReportPath == "" && o.ReportWriter == nil {
		return errors.New("shadow: mode is compare but no report destination is set: " +
			"an empty diff report is the completion criterion (§14.1), and a report " +
			"that is never written is not empty, it is absent")
	}
	for _, f := range o.IgnoreFields {
		if strings.TrimSpace(f) == "" {
			return errors.New("shadow: compare.ignore_fields contains an empty entry")
		}
	}
	return nil
}

// baseURL splits ReferenceURL into the parts a request is built from.
func (o *Options) baseURL() (scheme, host, prefix string) {
	u, err := url.Parse(o.ReferenceURL)
	if err != nil {
		return "", "", ""
	}
	return u.Scheme, u.Host, strings.TrimSuffix(u.Path, "/")
}
