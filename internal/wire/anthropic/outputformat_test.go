package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
)

// A schema crosses to Anthropic as a schema where the deployment can take one.
//
// This branch used to report the loss unconditionally, on the grounds that the
// only way to express a schema was to synthesize a tool and force `tool_choice`
// at it — a REWRITE of the request rather than a translation, which §10.1
// declines to invent. That reasoning was right and its premise expired:
// Anthropic grew `output_format`, a field-for-field counterpart of
// `response_format: {type: json_schema}`. Sending it changes nothing about what
// the model is asked to do.
func TestAJSONSchemaIsTranslatedWhereTheDeploymentTakesOne(t *testing.T) {
	req := &canonical.Request{
		Model:     "claude-x",
		Messages:  []canonical.Message{canonical.TextMessage(canonical.RoleUser, "hi")},
		MaxTokens: intPtr(16),
		ResponseFormat: &canonical.ResponseFormat{
			Kind:   canonical.FormatJSONSchema,
			Name:   "event",
			Schema: json.RawMessage(`{"type":"object","properties":{"when":{"type":"string"}},"required":["when"]}`),
		},
	}
	loss := &canonical.LossReport{}
	out, err := EncodeRequest(req, &EncodeOptions{
		Capabilities: DefaultCapabilities | canonical.CapJSONSchema,
		Loss:         loss,
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if out.OutputFormat == nil {
		t.Fatal("a deployment declaring CapJSONSchema received no output_format; the schema " +
			"was dropped on a target that can express it")
	}
	if out.OutputFormat.Type != canonical.FormatJSONSchema {
		t.Errorf("output_format.type = %q, want %q", out.OutputFormat.Type, canonical.FormatJSONSchema)
	}
	var got map[string]any
	if err := json.Unmarshal(out.OutputFormat.Schema, &got); err != nil {
		t.Fatalf("schema is not JSON: %v", err)
	}
	if got["type"] != "object" || got["required"] == nil {
		t.Errorf("the constraint did not survive: %v", got)
	}
	if len(loss.Dropped) != 0 || len(loss.Downgrades) != 0 {
		t.Errorf("a translated schema reported a loss: dropped=%v downgraded=%v",
			loss.Dropped, loss.Downgrades)
	}
}

// Without the capability it is still reported, because the alternative is a
// rewrite.
//
// Synthesizing a tool from the schema and forcing `tool_choice` at it makes the
// model call a function instead of answering: the reply arrives as a tool call,
// and a caller reading `choices[].message.content` finds it empty. That is a
// different request from the one that was sent, and §10.1 reports a loss rather
// than inventing one.
func TestWithoutTheCapabilityTheSchemaIsStillReported(t *testing.T) {
	req := &canonical.Request{
		Model:     "claude-x",
		Messages:  []canonical.Message{canonical.TextMessage(canonical.RoleUser, "hi")},
		MaxTokens: intPtr(16),
		ResponseFormat: &canonical.ResponseFormat{
			Kind:   canonical.FormatJSONSchema,
			Schema: json.RawMessage(`{"type":"object"}`),
		},
	}
	loss := &canonical.LossReport{}
	out, err := EncodeRequest(req, &EncodeOptions{Capabilities: DefaultCapabilities, Loss: loss})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if out.OutputFormat != nil {
		t.Error("output_format was sent to a deployment that never declared it could take one")
	}
	if len(loss.Downgrades) == 0 {
		t.Error("the schema vanished without being reported")
	}
}

// `$defs` are inlined, because Anthropic does not resolve references.
//
// Every schema generator factors shared shapes into `$defs`, so a translation
// that forwarded them verbatim would hand the upstream dangling pointers and
// turn a working request into a 400.
func TestSharedDefinitionsAreInlined(t *testing.T) {
	schema := `{
	  "type":"object",
	  "$defs":{"Addr":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}},
	  "properties":{"home":{"$ref":"#/$defs/Addr"},"work":{"$ref":"#/$defs/Addr"}},
	  "required":["home"]
	}`
	out, err := anthropicOutputSchema(json.RawMessage(schema))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "$ref") || strings.Contains(s, "$defs") {
		t.Errorf("references survived into the wire schema:\n%s", s)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	props := got["properties"].(map[string]any)
	for _, k := range []string{"home", "work"} {
		m, ok := props[k].(map[string]any)
		if !ok || m["type"] != "object" {
			t.Errorf("%s was not inlined: %v", k, props[k])
			continue
		}
		if m["required"] == nil {
			t.Errorf("%s lost the constraint the definition carried", k)
		}
	}
}

// A schema that refers to itself is reported, not truncated.
//
// A tree whose children are the same node cannot be inlined at all. Cutting it
// off at a depth limit would send a DIFFERENT contract than the caller wrote,
// and they would have no way to find out — so the loss is reported instead.
func TestASelfReferentialSchemaIsReportedRatherThanTruncated(t *testing.T) {
	schema := `{"type":"object","$defs":{"Node":{"type":"object","properties":{"child":{"$ref":"#/$defs/Node"}}}},
	            "properties":{"root":{"$ref":"#/$defs/Node"}}}`
	if _, err := anthropicOutputSchema(json.RawMessage(schema)); err == nil {
		t.Error("a cyclic schema rendered without complaint; whatever was sent is not the " +
			"contract the caller wrote")
	}
}

// A remote reference is refused rather than fetched.
//
// Resolving `$ref: "https://…"` would make this gateway fetch a document the
// caller named, on the request path — server-side request forgery with extra
// steps.
func TestARemoteReferenceIsNotFetched(t *testing.T) {
	_, err := anthropicOutputSchema(json.RawMessage(
		`{"type":"object","properties":{"x":{"$ref":"https://example.invalid/schema.json"}}}`))
	if err == nil {
		t.Fatal("a remote $ref was accepted")
	}
	if !strings.Contains(err.Error(), "local") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// Removing a keyword must not change which documents validate.
//
// Stripping is the one thing this file does silently, and the line is that only
// DESCRIPTIVE keywords go. Anything that narrows the accepted set stays, or the
// caller's contract has been altered without telling them.
func TestOnlyDescriptiveKeywordsAreRemoved(t *testing.T) {
	out, err := anthropicOutputSchema(json.RawMessage(`{
	  "type":"object","title":"T","description":"d","$comment":"c","default":{},
	  "properties":{"n":{"type":"integer","minimum":1,"maximum":9,"description":"x"}},
	  "required":["n"],"additionalProperties":false
	}`))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(out)
	for _, gone := range []string{"title", "description", "$comment", "default"} {
		if strings.Contains(s, `"`+gone+`"`) {
			t.Errorf("%s survived; it is documentation and Anthropic rejects it", gone)
		}
	}
	for _, kept := range []string{"minimum", "maximum", "required", "additionalProperties", "type"} {
		if !strings.Contains(s, `"`+kept+`"`) {
			t.Errorf("%s was removed; it changes which documents validate, so the caller's "+
				"contract is not the one that reached the model", kept)
		}
	}
}

func intPtr(n int) *int { return &n }
