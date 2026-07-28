package metrics

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Family is one parsed metric family.
type Family struct {
	Name    string
	Type    Type
	Help    string
	Samples []ParsedSample
}

// ParsedSample is one series and its value.
type ParsedSample struct {
	// Name is the sample's own name, which for a histogram carries the
	// _bucket, _sum or _count suffix.
	Name   string
	Labels map[string]string
	Value  float64
}

// Label returns one label value.
func (s ParsedSample) Label(name string) string { return s.Labels[name] }

// Parse reads an exposition body back.
//
// It exists so that tests assert against the parsed structure rather than
// against substrings. A substring assertion passes on a scrape Prometheus would
// reject — a missing TYPE line, a duplicated family, an unescaped quote in a
// label — which makes it exactly the wrong tool for checking an exposition
// format.
func Parse(body []byte) ([]Family, error) {
	var (
		out    []Family
		byName = map[string]*Family{}
		order  []string
	)
	get := func(name string) *Family {
		f, ok := byName[name]
		if !ok {
			f = &Family{Name: name}
			byName[name] = f
			order = append(order, name)
		}
		return f
	}

	seenHelp := map[string]bool{}
	seenType := map[string]bool{}

	for lineNo, line := range strings.Split(string(body), "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "# HELP ") {
			rest := line[len("# HELP "):]
			name, help, _ := strings.Cut(rest, " ")
			if seenHelp[name] {
				return nil, fmt.Errorf("line %d: duplicate HELP for %q", lineNo+1, name)
			}
			seenHelp[name] = true
			get(name).Help = help
			continue
		}
		if strings.HasPrefix(line, "# TYPE ") {
			rest := line[len("# TYPE "):]
			name, typ, ok := strings.Cut(rest, " ")
			if !ok {
				return nil, fmt.Errorf("line %d: malformed TYPE line %q", lineNo+1, line)
			}
			if seenType[name] {
				return nil, fmt.Errorf("line %d: duplicate TYPE for %q, which makes "+
					"Prometheus reject the whole scrape", lineNo+1, name)
			}
			seenType[name] = true
			t, err := parseType(typ)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNo+1, err)
			}
			get(name).Type = t
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}

		s, family, err := parseSample(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo+1, err)
		}
		f, ok := byName[family]
		if !ok {
			return nil, fmt.Errorf("line %d: sample %q has no HELP or TYPE line", lineNo+1, s.Name)
		}
		f.Samples = append(f.Samples, s)
	}

	for _, name := range order {
		out = append(out, *byName[name])
	}
	return out, nil
}

func parseType(s string) (Type, error) {
	switch s {
	case "counter":
		return Counter, nil
	case "gauge":
		return Gauge, nil
	case "histogram":
		return Histogram, nil
	}
	return 0, fmt.Errorf("unknown metric type %q", s)
}

// parseSample splits one sample line and reports which family it belongs to.
func parseSample(line string) (ParsedSample, string, error) {
	name := line
	labels := map[string]string{}

	if i := strings.IndexByte(line, '{'); i >= 0 {
		j := strings.LastIndexByte(line, '}')
		if j < i {
			return ParsedSample{}, "", fmt.Errorf("unbalanced braces in %q", line)
		}
		name = line[:i]
		var err error
		labels, err = parseLabels(line[i+1 : j])
		if err != nil {
			return ParsedSample{}, "", err
		}
		line = line[j+1:]
	} else {
		k := strings.IndexByte(line, ' ')
		if k < 0 {
			return ParsedSample{}, "", fmt.Errorf("no value in %q", line)
		}
		name = line[:k]
		line = line[k:]
	}

	v, err := strconv.ParseFloat(strings.TrimSpace(line), 64)
	if err != nil {
		return ParsedSample{}, "", fmt.Errorf("bad value in %q: %w", line, err)
	}

	family := name
	for _, suffix := range [...]string{"_bucket", "_sum", "_count"} {
		if strings.HasSuffix(name, suffix) {
			family = strings.TrimSuffix(name, suffix)
			break
		}
	}
	return ParsedSample{Name: name, Labels: labels, Value: v}, family, nil
}

// parseLabels reads a label block, honouring the escapes the writer emits.
func parseLabels(s string) (map[string]string, error) {
	out := map[string]string{}
	for len(s) > 0 {
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			return nil, fmt.Errorf("malformed label block %q", s)
		}
		name := s[:eq]
		s = s[eq+1:]
		if len(s) == 0 || s[0] != '"' {
			return nil, fmt.Errorf("label %q is not quoted", name)
		}
		s = s[1:]
		var val strings.Builder
		closed := false
		for i := 0; i < len(s); i++ {
			c := s[i]
			if c == '\\' && i+1 < len(s) {
				i++
				switch s[i] {
				case 'n':
					val.WriteByte('\n')
				default:
					val.WriteByte(s[i])
				}
				continue
			}
			if c == '"' {
				s = s[i+1:]
				closed = true
				break
			}
			val.WriteByte(c)
		}
		if !closed {
			return nil, fmt.Errorf("unterminated label value for %q", name)
		}
		out[name] = val.String()
		s = strings.TrimPrefix(s, ",")
	}
	return out, nil
}

// Validate parses a scrape and enforces the naming and typing rules of the
// package comment.
//
// The rules are not stylistic. `_total` on a gauge invites a `rate()` that
// silently produces nonsense; `_percent` on a 0–1 fraction is the exact trap
// VLLM.md §3.1 records in vLLM's own metrics, where the name and the
// documentation string disagree by a factor of a hundred; a family with no TYPE
// line is untyped to Prometheus and loses its aggregation rules.
func Validate(body []byte) []error {
	fams, err := Parse(body)
	if err != nil {
		return []error{err}
	}
	var errs []error
	for _, f := range fams {
		if f.Help == "" {
			errs = append(errs, fmt.Errorf("%s: no HELP line", f.Name))
		}
		if len(f.Samples) == 0 {
			errs = append(errs, fmt.Errorf("%s: HELP and TYPE with no samples; an empty "+
				"family is how 'unconfigured' gets read as 'zero'", f.Name))
		}
		if !strings.HasPrefix(f.Name, "dorang_") {
			errs = append(errs, fmt.Errorf("%s: not in the dorang_ namespace", f.Name))
		}

		switch {
		case strings.HasSuffix(f.Name, "_total"):
			if f.Type != Counter {
				errs = append(errs, fmt.Errorf("%s: named _total but typed %s; a rate() "+
					"over a non-counter is nonsense", f.Name, f.Type))
			}
		case strings.HasSuffix(f.Name, "_ratio"):
			if f.Type != Gauge {
				errs = append(errs, fmt.Errorf("%s: a ratio must be a gauge, got %s", f.Name, f.Type))
			}
			for _, s := range f.Samples {
				if s.Value < 0 || s.Value > 1 {
					errs = append(errs, fmt.Errorf("%s: value %v is outside [0,1]; a _ratio "+
						"that is really a percentage is the kv_cache_usage_perc mistake",
						f.Name, s.Value))
				}
			}
		case strings.HasSuffix(f.Name, "_percent"):
			for _, s := range f.Samples {
				if s.Value < 0 || s.Value > 100 {
					errs = append(errs, fmt.Errorf("%s: value %v is outside [0,100]",
						f.Name, s.Value))
				}
			}
		}

		if f.Type == Counter && !strings.HasSuffix(f.Name, "_total") &&
			!strings.HasSuffix(f.Name, "_seconds") && !strings.HasSuffix(f.Name, "_bytes") {
			errs = append(errs, fmt.Errorf("%s: a counter should end _total", f.Name))
		}
		if f.Type == Histogram && !strings.HasSuffix(f.Name, "_seconds") &&
			!strings.HasSuffix(f.Name, "_bytes") {
			errs = append(errs, fmt.Errorf("%s: a histogram must name its unit", f.Name))
		}

		// Every sample of a family must carry the same label names. Prometheus
		// tolerates a ragged family, and a dashboard built on one breaks the
		// first time the missing label shows up.
		if err := checkLabelShape(f); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

func checkLabelShape(f Family) error {
	var want []string
	for _, s := range f.Samples {
		names := make([]string, 0, len(s.Labels))
		for k := range s.Labels {
			if f.Type == Histogram && k == "le" {
				continue
			}
			names = append(names, k)
		}
		sort.Strings(names)
		if want == nil {
			want = names
			continue
		}
		if strings.Join(want, ",") != strings.Join(names, ",") {
			return fmt.Errorf("%s: ragged label set, %q then %q",
				f.Name, strings.Join(want, ","), strings.Join(names, ","))
		}
	}
	return nil
}
