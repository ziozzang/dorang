package pricing

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// whenPred is the compiled form of a rule's `when:` block. Everything that can be decided
// at load time — parsing the window, loading the location, turning weekday names into a
// bitmask — is decided at load time; only the comparison against the request's instant
// happens per request (§8.2).
type whenPred struct {
	loc *time.Location

	hasTOD   bool
	todStart int32 // minutes since midnight, inclusive
	todEnd   int32 // exclusive; wraps midnight when todEnd <= todStart

	weekdays uint8 // bit i set means time.Weekday(i) is allowed; 0 means any

	hasFrom bool
	from    time.Time
	hasTo   bool
	to      time.Time
}

var weekdayNames = map[string]time.Weekday{
	"sun": time.Sunday, "sunday": time.Sunday,
	"mon": time.Monday, "monday": time.Monday,
	"tue": time.Tuesday, "tues": time.Tuesday, "tuesday": time.Tuesday,
	"wed": time.Wednesday, "weds": time.Wednesday, "wednesday": time.Wednesday,
	"thu": time.Thursday, "thur": time.Thursday, "thurs": time.Thursday, "thursday": time.Thursday,
	"fri": time.Friday, "friday": time.Friday,
	"sat": time.Saturday, "saturday": time.Saturday,
}

func compileWhen(raw *rawWhen, def *time.Location) (*whenPred, error) {
	w := &whenPred{loc: def}
	if raw.TZ != "" {
		loc, err := time.LoadLocation(raw.TZ)
		if err != nil {
			return nil, fmt.Errorf("when.tz: %w", err)
		}
		w.loc = loc
	}
	if w.loc == nil {
		w.loc = time.UTC
	}
	if raw.TimeOfDay != "" {
		start, end, err := parseWindow(raw.TimeOfDay)
		if err != nil {
			return nil, err
		}
		w.hasTOD, w.todStart, w.todEnd = true, start, end
	}
	for _, name := range raw.Weekday {
		d, ok := weekdayNames[strings.ToLower(strings.TrimSpace(name))]
		if !ok {
			return nil, fmt.Errorf("when.weekday: unknown day %q", name)
		}
		w.weekdays |= 1 << uint(d)
	}
	if raw.DateRange != "" {
		from, to, err := parseDateRange(raw.DateRange, w.loc)
		if err != nil {
			return nil, err
		}
		w.hasFrom, w.from = !from.IsZero(), from
		w.hasTo, w.to = !to.IsZero(), to
	}
	if !w.hasTOD && w.weekdays == 0 && !w.hasFrom && !w.hasTo {
		return nil, errors.New("when: no predicate declared")
	}
	return w, nil
}

// parseWindow parses "HH:MM-HH:MM". The start is inclusive, the end exclusive, and a
// window whose end is not after its start wraps midnight.
func parseWindow(s string) (int32, int32, error) {
	dash := strings.IndexByte(s, '-')
	if dash < 0 {
		return 0, 0, fmt.Errorf("when.time_of_day: %q is not HH:MM-HH:MM", s)
	}
	start, err := parseClock(strings.TrimSpace(s[:dash]))
	if err != nil {
		return 0, 0, err
	}
	end, err := parseClock(strings.TrimSpace(s[dash+1:]))
	if err != nil {
		return 0, 0, err
	}
	if start == end {
		return 0, 0, fmt.Errorf("when.time_of_day: %q is an empty window", s)
	}
	return start, end, nil
}

func parseClock(s string) (int32, error) {
	colon := strings.IndexByte(s, ':')
	if colon < 0 {
		return 0, fmt.Errorf("when.time_of_day: %q is not HH:MM", s)
	}
	h, err := parseSmallInt(s[:colon])
	if err != nil || h > 24 {
		return 0, fmt.Errorf("when.time_of_day: bad hour in %q", s)
	}
	m, err := parseSmallInt(s[colon+1:])
	if err != nil || m > 59 {
		return 0, fmt.Errorf("when.time_of_day: bad minute in %q", s)
	}
	if h == 24 && m != 0 {
		return 0, fmt.Errorf("when.time_of_day: bad hour in %q", s)
	}
	return int32(h)*60 + int32(m), nil
}

func parseSmallInt(s string) (int, error) {
	if s == "" || len(s) > 2 || !allDigits(s) {
		return 0, errBadDecimal
	}
	v := 0
	for i := 0; i < len(s); i++ {
		v = v*10 + int(s[i]-'0')
	}
	return v, nil
}

// parseDateRange parses "YYYY-MM-DD..YYYY-MM-DD"; either side may be omitted. Both dates
// are inclusive, evaluated in the rule's location.
func parseDateRange(s string, loc *time.Location) (time.Time, time.Time, error) {
	i := strings.Index(s, "..")
	if i < 0 {
		return time.Time{}, time.Time{}, fmt.Errorf("when.date_range: %q is not FROM..TO", s)
	}
	var from, to time.Time
	if f := strings.TrimSpace(s[:i]); f != "" {
		t, err := time.ParseInLocation("2006-01-02", f, loc)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("when.date_range: %w", err)
		}
		from = t
	}
	if t := strings.TrimSpace(s[i+2:]); t != "" {
		d, err := time.ParseInLocation("2006-01-02", t, loc)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("when.date_range: %w", err)
		}
		to = d.AddDate(0, 0, 1) // inclusive end date
	}
	if !from.IsZero() && !to.IsZero() && !to.After(from) {
		return time.Time{}, time.Time{}, fmt.Errorf("when.date_range: %q ends before it starts", s)
	}
	return from, to, nil
}

// eligible reports whether the request's instant satisfies the rule's time predicates.
// This is the only matching work left for request time.
func (w *whenPred) eligible(at time.Time) bool {
	if w == nil {
		return true
	}
	local := at.In(w.loc)
	if w.weekdays != 0 && w.weekdays&(1<<uint(local.Weekday())) == 0 {
		return false
	}
	if w.hasTOD {
		m := int32(local.Hour())*60 + int32(local.Minute())
		if w.todStart < w.todEnd {
			if m < w.todStart || m >= w.todEnd {
				return false
			}
		} else if m < w.todStart && m >= w.todEnd {
			// Wraps midnight: [start, 24:00) union [00:00, end).
			return false
		}
	}
	if w.hasFrom && local.Before(w.from) {
		return false
	}
	if w.hasTo && !local.Before(w.to) {
		return false
	}
	return true
}

// selectWinner returns the most specific time-eligible rule of a class, or nil.
//
// The index means at most one bucket per specificity level is touched, and buckets are
// pre-sorted, so the common case examines one rule. examined counts every rule actually
// inspected; it is what proves the index is doing its job.
func (ix *classIndex) selectWinner(req *Request, at time.Time, examined *int, tr *ClassTrace) *rule {
	if ix.count == 0 {
		return nil
	}
	if req.Credential != "" {
		if r := pick(ix.byCredential[req.Credential], req, at, examined, tr); r != nil {
			return r
		}
	}
	if req.Deployment != "" {
		if r := pick(ix.byDeployment[req.Deployment], req, at, examined, tr); r != nil {
			return r
		}
	}
	if req.Provider != "" && req.Model != "" {
		if r := pick(ix.byProvModel[provModelKey{req.Provider, req.Model}], req, at, examined, tr); r != nil {
			return r
		}
	}
	if req.Model != "" {
		if r := pick(ix.byModel[req.Model], req, at, examined, tr); r != nil {
			return r
		}
	}
	if r := ix.pickPrefix(req, at, examined, tr); r != nil {
		return r
	}
	if req.Provider != "" {
		if r := pick(ix.byProvider[req.Provider], req, at, examined, tr); r != nil {
			return r
		}
	}
	return pick(ix.defaults, req, at, examined, tr)
}

// pick returns the first rule in a pre-sorted, single-level bucket that fully matches.
func pick(bucket []*rule, req *Request, at time.Time, examined *int, tr *ClassTrace) *rule {
	for _, r := range bucket {
		*examined++
		ok := r.staticMatches(req) && r.when.eligible(at)
		if tr != nil {
			tr.record(r, req, ok, false)
		}
		if ok {
			if tr != nil {
				tr.markSelected(r.id)
			}
			return r
		}
	}
	return nil
}

// pickPrefix scans the two buckets that can hold a prefix rule matching this model name
// and returns the best of them. Longer prefixes are more specific; priority and id break
// the rest of the tie.
func (ix *classIndex) pickPrefix(req *Request, at time.Time, examined *int, tr *ClassTrace) *rule {
	if len(ix.byPrefix) == 0 || req.Model == "" {
		return nil
	}
	var best *rule
	keys := [2]string{prefixKey(req.Model), req.Model[:1]}
	n := 1
	if keys[1] != keys[0] {
		n = 2
	}
	for i := 0; i < n; i++ {
		for _, r := range ix.byPrefix[keys[i]] {
			*examined++
			ok := r.staticMatches(req) && r.when.eligible(at)
			if tr != nil {
				tr.record(r, req, ok, false)
			}
			if !ok {
				continue
			}
			if best == nil || betterPrefix(r, best) {
				best = r
			}
			break // buckets are pre-sorted; the first eligible rule is that bucket's best.
		}
	}
	if best != nil && tr != nil {
		tr.markSelected(best.id)
	}
	return best
}

// collectAll appends every matching rule of a class, at every level, to dst. Adjustments
// do not compete: all of them apply, in order (§8.1).
func (ix *classIndex) collectAll(dst []*rule, req *Request, at time.Time, examined *int, tr *ClassTrace) []*rule {
	if ix.count == 0 {
		return dst
	}
	add := func(bucket []*rule) {
		for _, r := range bucket {
			*examined++
			ok := r.staticMatches(req) && r.when.eligible(at)
			if tr != nil {
				tr.record(r, req, ok, ok)
			}
			if ok {
				dst = append(dst, r)
			}
		}
	}
	if req.Credential != "" {
		add(ix.byCredential[req.Credential])
	}
	if req.Deployment != "" {
		add(ix.byDeployment[req.Deployment])
	}
	if req.Provider != "" && req.Model != "" {
		add(ix.byProvModel[provModelKey{req.Provider, req.Model}])
	}
	if req.Model != "" {
		add(ix.byModel[req.Model])
	}
	if len(ix.byPrefix) > 0 && req.Model != "" {
		keys := [2]string{prefixKey(req.Model), req.Model[:1]}
		add(ix.byPrefix[keys[0]])
		if keys[1] != keys[0] {
			add(ix.byPrefix[keys[1]])
		}
	}
	if req.Provider != "" {
		add(ix.byProvider[req.Provider])
	}
	add(ix.defaults)
	return dst
}

// record appends one examined rule to the trace. This is the Explain path only; Price
// passes a nil trace and builds no strings.
func (t *ClassTrace) record(r *rule, req *Request, eligible, selected bool) {
	var reason string
	switch {
	case selected:
		reason = "applies"
	case eligible:
		reason = "matched, but a more specific rule of this class won"
	case !r.staticMatches(req):
		reason = whyStaticMismatch(r, req)
	default:
		reason = "its time predicate is not satisfied at this instant"
	}
	t.Considered = append(t.Considered, Considered{
		RuleID: r.id, Level: r.level, Priority: r.priority, Order: r.order,
		Eligible: eligible, Selected: selected, Reason: reason,
	})
}

func whyStaticMismatch(r *rule, req *Request) string {
	switch {
	case r.credential != "" && r.credential != req.Credential:
		return "credential " + r.credential + " != " + req.Credential
	case r.deployment != "" && r.deployment != req.Deployment:
		return "deployment " + r.deployment + " != " + req.Deployment
	case r.provider != "" && r.provider != req.Provider:
		return "provider " + r.provider + " != " + req.Provider
	case r.model != "" && r.model != req.Model:
		return "model " + r.model + " != " + req.Model
	case r.modelPrefix != "" && !strings.HasPrefix(req.Model, r.modelPrefix):
		return "model " + req.Model + " does not start with " + r.modelPrefix
	}
	return "did not match"
}

func (t *ClassTrace) markSelected(id string) {
	for i := range t.Considered {
		if t.Considered[i].RuleID == id {
			t.Considered[i].Selected = true
			t.Considered[i].Reason = "selected: most specific matching rule of its class"
		}
	}
}
