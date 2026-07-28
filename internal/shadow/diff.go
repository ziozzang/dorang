package shadow

import (
	"sort"
	"strconv"
	"strings"
)

// Diff is one structural difference. It names where, what kind, and both sides,
// because a report row that says only "responses differ" cannot be acted on.
type Diff struct {
	// Path locates the difference. "status", "header/<key>", "stream/frames",
	// "error/envelope", or a JSON path — "$.usage.prompt_tokens", or
	// "content|$.choices[].delta.content" for a stream frame of that kind.
	Path string `json:"path"`
	// Kind is the class of difference. See the Kind* constants.
	Kind string `json:"kind"`
	// Dorang and Reference are the two sides, rendered.
	Dorang    string `json:"dorang"`
	Reference string `json:"reference"`
	// Note explains a difference whose reading is not obvious, and is the place
	// a difference that may be model nondeterminism rather than a defect says
	// so. It is never used to soften a difference into a non-difference.
	Note string `json:"note,omitempty"`
}

// The difference kinds.
const (
	KindStatus        = "status"
	KindHeaderMissing = "header_missing"
	KindContentKind   = "content_kind"
	KindFieldMissing  = "field_missing"
	KindFieldType     = "type"
	KindFrameSequence = "frame_sequence"
	KindFrameKinds    = "frame_kinds"
	KindTerminator    = "terminator"
	KindStopReason    = "stop_reason"
	KindUsage         = "usage_presence"
	KindErrEnvelope   = "error_envelope"
	KindErrType       = "error_type"
	KindErrCode       = "error_code"
	KindUnparseable   = "unparseable"
	KindTransport     = "transport"
)

// Inconclusive records a dimension the comparison could not decide, and why.
//
// It is the reason an empty diff report can be believed. A comparison that
// silently skipped a case would make "zero diffs" mean "zero diffs among the
// cases we happened to handle", which is not a cutover criterion. So a skipped
// dimension is written to the same report, counted separately, and "clean" is
// reserved for a comparison that decided every dimension it looked at.
type Inconclusive struct {
	Dimension string `json:"dimension"`
	Reason    string `json:"reason"`
}

const absent = "absent"

// compareShapes produces the differences and the undecided dimensions between
// dorang's response and the reference's.
func compareShapes(d, r *shape) ([]Diff, []Inconclusive) {
	var (
		diffs []Diff
		inc   []Inconclusive
	)
	add := func(dd Diff) { diffs = append(diffs, dd) }

	if d.status != r.status {
		add(Diff{
			Path: "status", Kind: KindStatus,
			Dorang: strconv.Itoa(d.status), Reference: strconv.Itoa(r.status),
		})
	}

	for _, k := range symmetric(d.headerKeys, r.headerKeys) {
		add(Diff{
			Path: "header/" + k.name, Kind: KindHeaderMissing,
			Dorang: presence(k.inA), Reference: presence(k.inB),
		})
	}

	if d.content != r.content {
		add(Diff{
			Path: "body", Kind: KindContentKind,
			Dorang: d.content.String(), Reference: r.content.String(),
		})
	}

	// An unparseable body on one side and a parsed one on the other is a
	// difference. On both sides it is undecidable, and saying so is the only
	// honest answer — reporting "no differences" between two bodies neither of
	// which was read would be the exact failure this report must not have.
	switch {
	case d.parseErr != "" && r.parseErr != "":
		inc = append(inc, Inconclusive{
			Dimension: "body",
			Reason:    "neither body could be read: dorang: " + d.parseErr + "; reference: " + r.parseErr,
		})
		return diffs, inc
	case d.parseErr != "":
		add(Diff{Path: "body", Kind: KindUnparseable, Dorang: d.parseErr, Reference: "parsed"})
		return diffs, inc
	case r.parseErr != "":
		add(Diff{Path: "body", Kind: KindUnparseable, Dorang: "parsed", Reference: r.parseErr})
		return diffs, inc
	}

	if d.content == contentOther && r.content == contentOther {
		inc = append(inc, Inconclusive{
			Dimension: "body",
			Reason:    "body is neither JSON nor an event stream on either side; only status and headers were compared",
		})
		return diffs, inc
	}

	pathDiffs := comparePaths(d, r)
	diffs = append(diffs, pathDiffs...)

	if d.content == contentSSE || r.content == contentSSE {
		frameDiffs := compareFrames(d, r)
		diffs = append(diffs, frameDiffs...)
		if len(frameDiffs) == 0 && (d.truncated || r.truncated) {
			inc = append(inc, Inconclusive{
				Dimension: "stream/frames",
				Reason: "one or both bodies exceeded the capture window, so the frame " +
					"sequence was compared as a prefix and a suffix; the elided middle " +
					"was not compared",
			})
		}
		if d.terminator != r.terminator {
			diffs = append(diffs, Diff{
				Path: "stream/terminator", Kind: KindTerminator,
				Dorang: d.terminator, Reference: r.terminator,
			})
		}
	}

	if len(pathDiffs) == 0 && (d.truncated || r.truncated) {
		inc = append(inc, Inconclusive{
			Dimension: "body/paths",
			Reason: "one or both bodies exceeded the capture window, so the field set " +
				"is a subset; raise shadow.capture.head_bytes to decide this dimension",
		})
	}
	if len(pathDiffs) == 0 && (d.pathLimit || r.pathLimit) {
		inc = append(inc, Inconclusive{
			Dimension: "body/paths",
			Reason:    "the response exceeded the per-comparison path budget, so the field set is a subset",
		})
	}

	// finish_reason and stop_reason are compared by value: they are a bounded
	// vocabulary (COMPATIBILITY §4.1), a client branches on them, and §4.2a
	// records that a mismapping tells a caller a failed turn ended normally.
	for _, k := range symmetricSets(d.stopReasons, r.stopReasons) {
		note := ""
		if !k.inA || !k.inB {
			note = "a stop reason seen on one side only can be model " +
				"nondeterminism — one response used a tool and the other did " +
				"not — or a mapping defect; it needs adjudication, not dismissal"
		}
		diffs = append(diffs, Diff{
			Path: "stop_reason/" + k.name, Kind: KindStopReason,
			Dorang: presence(k.inA), Reference: presence(k.inB), Note: note,
		})
	}

	if d.usagePresent != r.usagePresent {
		diffs = append(diffs, Diff{
			Path: "usage", Kind: KindUsage,
			Dorang: presence(d.usagePresent), Reference: presence(r.usagePresent),
		})
	}

	if d.errEnvelope != r.errEnvelope {
		diffs = append(diffs, Diff{
			Path: "error/envelope", Kind: KindErrEnvelope,
			Dorang: orNone(d.errEnvelope), Reference: orNone(r.errEnvelope),
		})
	}
	if d.errType != r.errType {
		diffs = append(diffs, Diff{
			Path: "error/type", Kind: KindErrType,
			Dorang: orNone(d.errType), Reference: orNone(r.errType),
		})
	}
	if d.errCode != r.errCode {
		diffs = append(diffs, Diff{
			Path: "error/code", Kind: KindErrCode,
			Dorang: orNone(d.errCode), Reference: orNone(r.errCode),
		})
	}

	sort.SliceStable(diffs, func(i, j int) bool { return diffs[i].Path < diffs[j].Path })
	return diffs, inc
}

// comparePaths reports fields present on one side only and fields whose type
// set differs.
func comparePaths(d, r *shape) []Diff {
	var out []Diff
	seen := make(map[string]bool, len(d.paths)+len(r.paths))
	for p := range d.paths {
		seen[p] = true
	}
	for p := range r.paths {
		seen[p] = true
	}
	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, p := range paths {
		dt, dok := d.paths[p]
		rt, rok := r.paths[p]
		switch {
		case dok && !rok:
			out = append(out, Diff{Path: p, Kind: KindFieldMissing,
				Dorang: dt.String(), Reference: absent})
		case !dok && rok:
			out = append(out, Diff{Path: p, Kind: KindFieldMissing,
				Dorang: absent, Reference: rt.String()})
		case dt != rt:
			// A path whose type is not compared on either side is equal by
			// definition; one where only one side is marked cannot happen,
			// since the ignore set is the same for both.
			if dt&typeIgnored != 0 || rt&typeIgnored != 0 {
				continue
			}
			out = append(out, Diff{Path: p, Kind: KindFieldType,
				Dorang: dt.String(), Reference: rt.String()})
		}
	}
	return out
}

// compareFrames reports the two independent ways a stream's framing can differ.
//
// They are reported separately on purpose. A kind present on one side only can
// be model nondeterminism — one response made a tool call and the other did not
// — and needs a human to adjudicate. The same kinds in a different *order*
// cannot be nondeterminism: the protocol fixes the order, so an ordering
// difference is a defect. Collapsing both into "the streams differ" would make
// the first uninterpretable and the second easy to dismiss.
func compareFrames(d, r *shape) []Diff {
	var out []Diff
	for _, k := range symmetricSets(d.frameKinds, r.frameKinds) {
		out = append(out, Diff{
			Path: "stream/frames", Kind: KindFrameKinds,
			Dorang: presence(k.inA), Reference: presence(k.inB),
			Note: "frame kind " + k.name + " appears on one side only; this can be " +
				"model nondeterminism (a tool call on one side) or a framing defect",
		})
	}
	if !equalStrings(d.frames, r.frames) && sameSet(d.frameKinds, r.frameKinds) {
		out = append(out, Diff{
			Path: "stream/frames", Kind: KindFrameSequence,
			Dorang:    strings.Join(d.frames, ","),
			Reference: strings.Join(r.frames, ","),
			Note: "the same frame kinds in a different order; the protocol fixes " +
				"this order, so it is not nondeterminism",
		})
	}
	return out
}

type setEntry struct {
	name     string
	inA, inB bool
}

// symmetric reports the entries present in exactly one of two sorted key lists.
func symmetric(a, b []string) []setEntry {
	as := make(map[string]bool, len(a))
	for _, v := range a {
		as[v] = true
	}
	bs := make(map[string]bool, len(b))
	for _, v := range b {
		bs[v] = true
	}
	return symmetricSets(as, bs)
}

func symmetricSets(a, b map[string]bool) []setEntry {
	var out []setEntry
	for k := range a {
		if !b[k] {
			out = append(out, setEntry{name: k, inA: true})
		}
	}
	for k := range b {
		if !a[k] {
			out = append(out, setEntry{name: k, inB: true})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func sameSet(a, b map[string]bool) bool { return len(symmetricSets(a, b)) == 0 }

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func presence(in bool) string {
	if in {
		return "present"
	}
	return absent
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
