package systemone

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
)

// ScoringQuestion is one question in the shape a logprob scorer renders: the
// text it puts in front of the model and the candidates it reads back.
type ScoringQuestion struct {
	ID           string
	Type         string
	Instructions string
	// Names are the candidates' public names, in REQUEST order: the Choice's
	// option keys, a Score's level indexes ("0", "1", …), or "true"/"false"
	// for a Noul. Order matters — it is the order the model reads them in.
	Names []string
	// Details are the descriptions rendered as text, one per name; empty
	// where the request gave none (a null Choice option, an omitted Noul side).
	Details []string
}

// ParseForScoring reads a request the gateway already validated into the
// rendered state and its questions, with each Choice's options in the order
// the client wrote them.
func ParseForScoring(body []byte) (string, []ScoringQuestion, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return "", nil, err
	}
	var questions map[string]json.RawMessage
	if err := json.Unmarshal(top["questions"], &questions); err != nil {
		return "", nil, err
	}
	ids := make([]string, 0, len(questions))
	for id := range questions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]ScoringQuestion, 0, len(ids))
	for _, id := range ids {
		var q struct {
			Type         string          `json:"type"`
			Instructions json.RawMessage `json:"instructions"`
			Criteria     json.RawMessage `json:"criteria"`
		}
		if err := json.Unmarshal(questions[id], &q); err != nil {
			return "", nil, err
		}
		sq := ScoringQuestion{ID: id, Type: q.Type, Instructions: Render(q.Instructions)}
		switch q.Type {
		case "choice":
			keys, vals, err := orderedObject(q.Criteria)
			if err != nil {
				return "", nil, fmt.Errorf("questions.%s.criteria: %w", id, err)
			}
			sq.Names = keys
			for _, v := range vals {
				sq.Details = append(sq.Details, Render(v))
			}
		case "score":
			var levels []json.RawMessage
			if err := json.Unmarshal(q.Criteria, &levels); err != nil {
				return "", nil, err
			}
			for i, l := range levels {
				sq.Names = append(sq.Names, strconv.Itoa(i))
				sq.Details = append(sq.Details, Render(l))
			}
		case "noul":
			var c map[string]json.RawMessage
			if len(bytes.TrimSpace(q.Criteria)) > 0 {
				_ = json.Unmarshal(q.Criteria, &c)
			}
			sq.Names = []string{"true", "false"}
			sq.Details = []string{Render(c["true"]), Render(c["false"])}
		default:
			return "", nil, fmt.Errorf("questions.%s.type: unknown %q", id, q.Type)
		}
		out = append(out, sq)
	}
	return Render(top["state"]), out, nil
}

// Render turns a content value into the text a model reads: a string as
// itself, an object or array as compact JSON with sorted keys (so the same
// value always renders to the same bytes, which is what lets a prefix cache
// hit), null or absent as "".
func Render(raw json.RawMessage) string {
	b := bytes.TrimSpace(raw)
	if len(b) == 0 || string(b) == "null" {
		return ""
	}
	if b[0] == '"' {
		var s string
		if json.Unmarshal(b, &s) == nil {
			return s
		}
	}
	var v any
	if json.Unmarshal(b, &v) != nil {
		return string(b)
	}
	out, err := json.Marshal(v) // map keys marshal sorted
	if err != nil {
		return string(b)
	}
	return string(out)
}

// orderedObject returns an object's keys and values in document order.
func orderedObject(raw json.RawMessage) ([]string, []json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, nil, fmt.Errorf("must be an object")
	}
	var keys []string
	var vals []json.RawMessage
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		k, _ := kt.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, nil, err
		}
		keys = append(keys, k)
		vals = append(vals, v)
	}
	return keys, vals, nil
}

// ChoiceConfidence is how far the top option stands above a uniform guess,
// clamped to [0, 1]: (K·max(p) − 1) / (K − 1).
//
// It is the formula TypeSafe documents for a Choice
// (https://docs.typesafe.ai/confidence.md) and the one SGLang's System One
// route follows; checked against live Jev answers (0.83/0.17/0 → 0.745,
// reported 0.74).
func ChoiceConfidence(p []float64) float64 {
	n := len(p)
	if n < 2 {
		return 1
	}
	top := 0.0
	for _, x := range p {
		top = math.Max(top, x)
	}
	return clamp01((float64(n)*top - 1) / float64(n-1))
}

// ScoreConfidence is one minus the spread around the top level relative to a
// uniform distribution's spread, floored at 0:
//
//	1 − Σ p_i·|i − top| / ( Σ_i |i − (K−1)/2| / K )
//
// It is the formula SGLang's System One route uses for a Score, following
// TypeSafe; checked against a live Jev answer (0/0.01/0.99 → 0.985, reported
// 0.99).
func ScoreConfidence(p []float64) float64 {
	n := len(p)
	if n < 2 {
		return 1
	}
	top := 0
	for i := range p {
		if p[i] > p[top] {
			top = i
		}
	}
	var spread, uniform float64
	for i, x := range p {
		spread += x * math.Abs(float64(i-top))
		uniform += math.Abs(float64(i) - float64(n-1)/2)
	}
	uniform /= float64(n)
	if uniform == 0 {
		return 1
	}
	return clamp01(1 - spread/uniform)
}

func clamp01(x float64) float64 { return math.Max(0, math.Min(1, x)) }
