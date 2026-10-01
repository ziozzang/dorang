// Package systemone is the decision-model surface: POST /v1/systemone.
//
// # The contract
//
// TypeSafe defines it (https://docs.typesafe.ai/api.md, Jev). A request is one
// shared `state` and a map of named `questions`, each a Noul (a yes/no
// probability), a Choice (one option of a set, with a probability for every
// option) or a Score (a probability-weighted level on an ordered rubric). The
// answer carries one typed answer per question id, and token usage. Local
// Ollama serves the same shape at the same path
// (https://docs.ollama.com/api/systemone.md), with tighter limits, and
// aggregators expose other vendors' decision models through it.
//
// # What is checked here, and what is not
//
// The gateway checks STRUCTURE and the limits every implementation shares, and
// refuses with 422 — the status TypeSafe answers a malformed body with — naming
// the field. A request either implementation would accept is accepted here:
// Score levels up to 26 (Ollama's ceiling; TypeSafe's is 10) and Choice options
// up to 255 (TypeSafe's; Ollama's is 26). Each backend applies its own, tighter
// limits when the request reaches it, so a request a deployment cannot take is
// refused by that deployment and names that deployment's rule.
//
// Two rules are the gateway's own and are stricter than TypeSafe states: a
// Choice needs at least two options and a Score at least two levels. TypeSafe's
// documentation says a Score "should have at least two levels" and gives a
// Choice no minimum; Ollama requires two of each. A one-option choice is not a
// decision, and refusing it here is cheaper than a probability of 1.
package systemone

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/ziozzang/dorang/internal/canonical"
)

// Shared limits. See the package comment for which contract each comes from.
const (
	// MaxChoiceOptions is TypeSafe's ceiling on a Choice's options.
	MaxChoiceOptions = 255
	// MaxScoreLevels is Ollama's ceiling on a Score's levels; TypeSafe's is 10.
	MaxScoreLevels = 26
	// MinOptions is the gateway's floor for both a Choice and a Score.
	MinOptions = 2
)

// Error is a validation failure. Param names the offending field the way a
// client addresses it, e.g. "questions.route.criteria".
type Error struct {
	Param   string
	Message string
}

func (e *Error) Error() string { return e.Param + ": " + e.Message }

func fail(param, format string, args ...any) error {
	return &Error{Param: param, Message: fmt.Sprintf(format, args...)}
}

// ErrNotAResponse is an upstream 200 whose body is a JSON object with no
// `answers` member — an error envelope wearing a success status.
var ErrNotAResponse = errors.New("systemone: the body is a JSON object but not a System One answer")

// DecodeRequest validates a System One request body and returns what the
// backends need to know about it. The body itself is relayed unchanged.
func DecodeRequest(body []byte) (*canonical.SystemOneRequest, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil || top == nil {
		return nil, fail("body", "must be a JSON object")
	}

	var model string
	if err := json.Unmarshal(top["model"], &model); err != nil || strings.TrimSpace(model) == "" {
		return nil, fail("model", "is required and must be a non-blank string")
	}
	if err := checkContent("state", top["state"], false); err != nil {
		return nil, err
	}

	raw, ok := top["questions"]
	var questions map[string]json.RawMessage
	if !ok || json.Unmarshal(raw, &questions) != nil || questions == nil {
		return nil, fail("questions", "is required and must be an object of named questions")
	}
	if len(questions) == 0 {
		return nil, fail("questions", "must name at least one question")
	}

	out := &canonical.SystemOneRequest{Model: model, StringCriteriaOnly: true}
	ids := make([]string, 0, len(questions))
	for id := range questions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			return nil, fail("questions", "a question id must not be blank")
		}
		q, stringsOnly, err := decodeQuestion("questions."+id, questions[id])
		if err != nil {
			return nil, err
		}
		q.ID = id
		out.Questions = append(out.Questions, q)
		out.StringCriteriaOnly = out.StringCriteriaOnly && stringsOnly
	}
	return out, nil
}

func decodeQuestion(path string, raw json.RawMessage) (canonical.SystemOneQuestion, bool, error) {
	var q canonical.SystemOneQuestion
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return q, false, fail(path, "must be an object")
	}
	if err := json.Unmarshal(obj["type"], &q.Type); err != nil {
		return q, false, fail(path+".type", `is required: "noul", "choice" or "score"`)
	}
	if err := checkContent(path+".instructions", obj["instructions"], false); err != nil {
		return q, false, err
	}
	criteria, has := obj["criteria"]
	cpath := path + ".criteria"
	stringsOnly := true

	switch q.Type {
	case canonical.SystemOneNoul:
		if !has || isNull(criteria) {
			return q, true, nil
		}
		var c map[string]json.RawMessage
		if json.Unmarshal(criteria, &c) != nil || c == nil {
			return q, false, fail(cpath, `must be an object with optional "true" and "false" descriptions`)
		}
		for k, v := range c {
			if k != "true" && k != "false" {
				return q, false, fail(cpath, `has only "true" and "false" entries, not %q`, k)
			}
			if err := checkContent(cpath+"."+k, v, false); err != nil {
				return q, false, err
			}
			stringsOnly = stringsOnly && isString(v)
		}
		return q, stringsOnly, nil

	case canonical.SystemOneChoice:
		var c map[string]json.RawMessage
		if !has || json.Unmarshal(criteria, &c) != nil || c == nil {
			return q, false, fail(cpath, "is required: an object mapping each option to its description (null for none)")
		}
		if len(c) < MinOptions || len(c) > MaxChoiceOptions {
			return q, false, fail(cpath, "has %d options; a Choice takes %d to %d", len(c), MinOptions, MaxChoiceOptions)
		}
		for k, v := range c {
			if strings.TrimSpace(k) == "" {
				return q, false, fail(cpath, "an option key must not be blank")
			}
			if err := checkContent(cpath+"."+k, v, true); err != nil {
				return q, false, err
			}
			stringsOnly = stringsOnly && (isString(v) || isNull(v))
		}
		q.Options = len(c)
		return q, stringsOnly, nil

	case canonical.SystemOneScore:
		var c []json.RawMessage
		if !has || json.Unmarshal(criteria, &c) != nil || c == nil {
			return q, false, fail(cpath, "is required: an array of level descriptions, lowest first")
		}
		if len(c) < MinOptions || len(c) > MaxScoreLevels {
			return q, false, fail(cpath, "has %d levels; a Score takes %d to %d", len(c), MinOptions, MaxScoreLevels)
		}
		for i, v := range c {
			if err := checkContent(fmt.Sprintf("%s[%d]", cpath, i), v, false); err != nil {
				return q, false, err
			}
			stringsOnly = stringsOnly && isString(v)
		}
		q.Options = len(c)
		return q, stringsOnly, nil
	}
	return q, false, fail(path+".type", `must be "noul", "choice" or "score", not %q`, q.Type)
}

// checkContent requires a string, an object or an array — the "content" type
// both contracts use for state, instructions and criteria. allowNull admits
// JSON null, which a Choice option uses for "no description".
func checkContent(path string, raw json.RawMessage, allowNull bool) error {
	b := bytes.TrimSpace(raw)
	if len(b) == 0 {
		return fail(path, "is required")
	}
	switch b[0] {
	case '"', '{', '[':
		return nil
	case 'n':
		if allowNull && string(b) == "null" {
			return nil
		}
	}
	return fail(path, "must be a string, an object or an array")
}

func isString(raw json.RawMessage) bool {
	b := bytes.TrimSpace(raw)
	return len(b) > 0 && b[0] == '"'
}

func isNull(raw json.RawMessage) bool { return string(bytes.TrimSpace(raw)) == "null" }

// Usage is a System One answer's token counts.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// CheckResponse reads an upstream answer: it must be a JSON object carrying an
// `answers` object, and its `usage` is returned when present.
func CheckResponse(body []byte) (Usage, bool, error) {
	var shape struct {
		Answers json.RawMessage `json:"answers"`
		Usage   *Usage          `json:"usage"`
	}
	if err := json.Unmarshal(body, &shape); err != nil {
		return Usage{}, false, err
	}
	if b := bytes.TrimSpace(shape.Answers); len(b) == 0 || b[0] != '{' {
		return Usage{}, false, ErrNotAResponse
	}
	if shape.Usage == nil {
		return Usage{}, false, nil
	}
	return *shape.Usage, true, nil
}
