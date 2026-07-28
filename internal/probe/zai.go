package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/ziozzang/dorang/internal/quota"
)

// z.ai publishes a coding-plan credential's own quota, which makes it the one
// provider in this package that supplies everything DESIGN §7.5a(c) needs: a
// window length, a consumed proportion, and — the load-bearing one — the
// instant the window resets. A rolling window has no reset of its own, so
// urgency scores zero until a provider reports one.
//
// The endpoint is undocumented. Z.AI's own developer FAQ describes no
// programmatic quota API and points at the web console, so the shape below is
// what the endpoint is observed to return rather than what a vendor promised,
// and every field is optional-tolerant for that reason. Two things follow from
// it, both encoded in the decoder rather than left to a comment: an entry whose
// window length is not described is carried unmapped rather than filed under a
// guessed length, and TIME_LIMIT is carried unmapped because sources disagree
// about what it counts.
const (
	zaiBaseURL = "https://api.z.ai"
	zaiPath    = "/api/monitor/usage/quota/limit"
)

type zaiSource struct{}

func (zaiSource) name() string { return "zai" }

func (zaiSource) endpoint(base string) string { return baseOr(base, zaiBaseURL) + zaiPath }

func (zaiSource) accepts(string) error { return nil }

func (s zaiSource) request(ctx context.Context, base, token, ua string) (*http.Request, error) {
	return newRequest(ctx, s.endpoint(base), ua, http.Header{
		"Authorization": {"Bearer " + token},
		// The endpoint localizes its messages. Asking for English keeps a
		// diagnostic readable; nothing in the decode path depends on it.
		"Accept-Language": {"en-US,en"},
	})
}

// zaiEnvelope is the response wrapper. code is observed as both 0 and 200 by
// different integrations, so success is what is checked when it is present and
// the code is only reported.
type zaiEnvelope struct {
	Code    number   `json:"code"`
	Success *bool    `json:"success"`
	Data    *zaiData `json:"data"`
}

type zaiData struct {
	Limits []zaiLimit `json:"limits"`
}

// zaiLimit is one reported window.
//
// percentage is consumed, 0–100. unit and number describe the window's length:
// unit 1 is a day, 3 an hour, 5 a minute, and number is how many. nextResetTime
// arrives as unix milliseconds from some deployments and as an ISO-8601 string
// from others, which is why it is a [resetTime].
type zaiLimit struct {
	Type          string    `json:"type"`
	Percentage    number    `json:"percentage"`
	Unit          number    `json:"unit"`
	Number        number    `json:"number"`
	NextResetTime resetTime `json:"nextResetTime"`
}

// z.ai's duration-unit enum.
const (
	zaiUnitDay    = 1
	zaiUnitHour   = 3
	zaiUnitMinute = 5
)

// zaiTypeTokens is the only limit type whose meaning is agreed on.
const (
	zaiTypeTokens = "TOKENS_LIMIT"
	zaiTypeTime   = "TIME_LIMIT"
)

// zaiTypes is the vocabulary a label may be built from; see [labelOf].
var zaiTypes = []string{zaiTypeTokens, zaiTypeTime}

func (zaiSource) decode(body []byte) (reading, error) {
	var env zaiEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		// The provider's parse error is not quoted: a body that fails to parse
		// is exactly the body most likely to be an error page with the key in
		// it.
		return reading{}, errors.New("response is not the expected JSON object")
	}
	if env.Success != nil && !*env.Success {
		code, _ := env.Code.Int()
		return reading{}, fmt.Errorf("provider reported failure (code %d)", code)
	}
	if env.Data == nil {
		return reading{}, errors.New("response carries no data object")
	}

	var r reading
	for _, l := range env.Data.Limits {
		r.windows = append(r.windows, zaiWindow(l))
	}
	return r, nil
}

// zaiWindow normalizes one limit entry.
func zaiWindow(l zaiLimit) Window {
	w := Window{ResetAt: l.NextResetTime.Time}
	if l.Percentage.Set {
		w.UsedPercent, w.Known = usedPercent(l.Percentage.Value)
	}

	kind := labelOf(zaiTypes, l.Type)
	w.Label = kind

	length, lengthKnown := zaiLength(l)
	if !lengthKnown {
		// The window's length is what makes the figure comparable to anything.
		// Without it there is no quota.Window to file under, and inventing one
		// would attach the reset instant to a rule it does not describe.
		return w
	}
	qw := quota.Rolling(length)
	w.Label = kind + ":" + qw.String()
	if !qw.Valid() {
		return w
	}
	w.Window = qw

	// The metric. Only TOKENS_LIMIT has an agreed meaning; TIME_LIMIT is
	// reported by one source as the time-windowed prompt allowance and by
	// another as a monthly count of web-search and MCP calls, which are
	// different quantities under one name. It is carried unmapped, with its
	// label and its reset intact, so an operator who knows which their plan
	// means can file it with an [Allowance] — and so that this package does not
	// file a tool-call count under a model-usage rule.
	if l.Type == zaiTypeTokens {
		w.Metric, w.Mapped = quota.MetricTokensTotal, true
	}
	return w
}

// zaiLength turns the unit/number pair into a duration.
//
// A missing number means one of the unit, which is how the shorter observed
// payload spells a single-unit window; a missing or unrecognized unit means the
// length is unknown, and the window is carried unmapped rather than guessed.
func zaiLength(l zaiLimit) (time.Duration, bool) {
	unit, ok := l.Unit.Int()
	if !ok {
		return 0, false
	}
	var d time.Duration
	switch unit {
	case zaiUnitDay:
		d = 24 * time.Hour
	case zaiUnitHour:
		d = time.Hour
	case zaiUnitMinute:
		d = time.Minute
	default:
		return 0, false
	}
	n := int64(1)
	if v, ok := l.Number.Int(); ok {
		n = v
	}
	if n <= 0 || n > int64(maxWindowLength/d) {
		return 0, false
	}
	return time.Duration(n) * d, true
}

// maxWindowLength refuses a window longer than any subscription period, which
// is what a unit confusion looks like from here.
const maxWindowLength = 366 * 24 * time.Hour
