package openai

import (
	"encoding/json"

	"github.com/ziozzang/dorang/internal/canonical"
)

// The audio surfaces: POST /v1/audio/speech, /v1/audio/transcriptions and
// /v1/audio/translations.
//
// Two of the three take multipart bodies and one returns bytes rather than
// JSON, which is why they are here as explicit adapters rather than as a relay:
// a gateway that special-cases "this one is not JSON" acquires a second
// response path that no conversion, metering or header rule applies to, and
// then diverges from the first one on every subsequent change.

// SpeechRequest is a text-to-speech request.
type SpeechRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
	// Voice is an opaque vendor id, for the same reason a model name is
	// (DESIGN §2.1): the values are vendor-specific and parsing one is how a
	// gateway starts rejecting the next voice a provider ships.
	Voice          string   `json:"voice"`
	Instructions   string   `json:"instructions,omitempty"`
	ResponseFormat string   `json:"response_format,omitempty"`
	Speed          *float64 `json:"speed,omitempty"`
	StreamFormat   string   `json:"stream_format,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

var speechRequestKnown = knownKeys("model", "input", "voice", "instructions",
	"response_format", "speed", "stream_format")

// MarshalJSON implements [encoding/json.Marshaler].
func (r SpeechRequest) MarshalJSON() ([]byte, error) {
	type alias SpeechRequest
	return marshalWithExtra(alias(r), r.Extra, speechRequestKnown)
}

// UnmarshalJSON implements [encoding/json.Unmarshaler] with case-SENSITIVE
// field matching (COMPATIBILITY 2.0).
func (r *SpeechRequest) UnmarshalJSON(b []byte) error {
	type alias SpeechRequest
	var a alias
	b = strictBytes(b, &a)
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	extra, err := splitExtra(b, speechRequestKnown)
	if err != nil {
		return err
	}
	*r = SpeechRequest(a)
	r.Extra = extra
	return nil
}

// DecodeSpeechRequest parses speech request bytes into the neutral form.
func DecodeSpeechRequest(b []byte) (*canonical.SpeechRequest, error) {
	var w SpeechRequest
	if err := strictUnmarshal(b, &w); err != nil {
		return nil, err
	}
	return &canonical.SpeechRequest{
		Model:        w.Model,
		Input:        w.Input,
		Voice:        w.Voice,
		Instructions: w.Instructions,
		Format:       w.ResponseFormat,
		Speed:        w.Speed,
		StreamFormat: w.StreamFormat,
		Extra:        w.Extra,
	}, nil
}

// MarshalSpeechRequest encodes a neutral speech request. model is the upstream
// id, which is how the real deployment name reaches the backend while the
// client-facing name stays in the answer (DESIGN §7.2).
func MarshalSpeechRequest(req *canonical.SpeechRequest, model string) ([]byte, error) {
	if req == nil {
		return nil, errNilRequest
	}
	w := SpeechRequest{
		Model:          req.Model,
		Input:          req.Input,
		Voice:          req.Voice,
		Instructions:   req.Instructions,
		ResponseFormat: req.Format,
		Speed:          req.Speed,
		StreamFormat:   req.StreamFormat,
		Extra:          req.Extra,
	}
	if model != "" {
		w.Model = model
	}
	return Marshal(w)
}

// SpeechMediaType is the response Content-Type for a speech container.
//
// The vendor labels each container correctly and dorang reproduces the same
// labelling rather than forwarding whatever the backend said, because a
// self-hosted engine routinely answers application/octet-stream and a browser
// handed that plays nothing.
func SpeechMediaType(format string) string {
	switch format {
	case "", "mp3":
		return "audio/mpeg"
	case "opus":
		return "audio/ogg"
	case "aac":
		return "audio/aac"
	case "flac":
		return "audio/flac"
	case "wav":
		return "audio/wav"
	case "pcm":
		return "audio/pcm"
	default:
		return "application/octet-stream"
	}
}

// ---------------------------------------------------------------------------
// Transcription and translation
// ---------------------------------------------------------------------------

// Transcription form field names.
const (
	FieldFile                   = "file"
	FieldModel                  = "model"
	FieldLanguage               = "language"
	FieldPrompt                 = "prompt"
	FieldResponseFormat         = "response_format"
	FieldTemperature            = "temperature"
	FieldTimestampGranularities = "timestamp_granularities"
	FieldInclude                = "include"
	FieldStream                 = "stream"
)

// transcriptionKnown is the set of form fields this adapter models. Everything
// else on the form is carried through Extra so a backend knob dorang has never
// heard of still reaches it.
var transcriptionKnown = map[string]struct{}{
	FieldFile: {}, FieldModel: {}, FieldLanguage: {}, FieldPrompt: {},
	FieldResponseFormat: {}, FieldTemperature: {}, FieldStream: {},
	FieldTimestampGranularities: {}, FieldTimestampGranularities + "[]": {},
	FieldInclude: {}, FieldInclude + "[]": {},
}

// DecodeTranscriptionForm converts a parsed multipart body into the neutral
// form. translate selects /v1/audio/translations semantics.
//
// Field lookup is an exact map lookup, so a field spelled "Model" is not this
// endpoint's model — the same rule the JSON paths get from
// [canonical.StrictBytes], and for the same reason (COMPATIBILITY 2.0).
func DecodeTranscriptionForm(f *canonical.Form, translate bool) (*canonical.TranscriptionRequest, error) {
	if f == nil {
		return nil, errNilRequest
	}
	file := f.File(FieldFile)
	if file == nil {
		return nil, errorString("openai: the multipart body carried no file part")
	}
	out := &canonical.TranscriptionRequest{
		Model:                  f.Get(FieldModel),
		File:                   *file,
		Translate:              translate,
		Prompt:                 f.Get(FieldPrompt),
		Format:                 f.Get(FieldResponseFormat),
		TimestampGranularities: f.All(FieldTimestampGranularities),
		Include:                f.All(FieldInclude),
	}
	if !translate {
		// The translation surface does not accept an input-language hint: the
		// output is English by definition. Forwarding it produces a 400 the
		// caller has never seen, so it is dropped here where the endpoint is
		// known rather than at the backend where it is not.
		out.Language = f.Get(FieldLanguage)
	}
	if v, ok := f.Float(FieldTemperature); ok {
		out.Temperature = &v
	}
	if v, ok := f.Bool(FieldStream); ok {
		out.Stream = v
	}
	for name, values := range f.Values {
		if _, known := transcriptionKnown[name]; known || len(values) == 0 {
			continue
		}
		if out.Extra == nil {
			out.Extra = make(map[string]string, 4)
		}
		out.Extra[name] = values[0]
	}
	return out, nil
}

// EncodeTranscriptionForm renders a neutral transcription request as a
// multipart body.
func EncodeTranscriptionForm(req *canonical.TranscriptionRequest, model, boundary string) ([]byte, string, error) {
	if req == nil {
		return nil, "", errNilRequest
	}
	f := &canonical.Form{}
	name := req.Model
	if model != "" {
		name = model
	}
	f.Set(FieldModel, name)
	if req.Language != "" && !req.Translate {
		f.Set(FieldLanguage, req.Language)
	}
	if req.Prompt != "" {
		f.Set(FieldPrompt, req.Prompt)
	}
	if req.Format != "" {
		f.Set(FieldResponseFormat, req.Format)
	}
	if req.Temperature != nil {
		f.Set(FieldTemperature, formatFloat(*req.Temperature))
	}
	for _, g := range req.TimestampGranularities {
		f.Add(FieldTimestampGranularities+"[]", g)
	}
	for _, g := range req.Include {
		f.Add(FieldInclude+"[]", g)
	}
	for k, v := range req.Extra {
		if _, known := transcriptionKnown[k]; !known {
			f.Set(k, v)
		}
	}
	file := req.File
	file.Field = FieldFile
	if file.Name == "" {
		// A backend infers the container from the filename, so an unnamed part
		// is refused by most of them. Supplying one is better than a 400 the
		// caller cannot act on.
		file.Name = "audio"
	}
	f.Files = []canonical.File{file}

	body, err := f.Encode(boundary, []string{
		FieldModel, FieldLanguage, FieldPrompt, FieldResponseFormat, FieldTemperature,
	})
	if err != nil {
		return nil, "", err
	}
	return body, boundary, nil
}

// TranscriptionResponse is the JSON answer of a transcription or translation.
type TranscriptionResponse struct {
	Text     string                     `json:"text"`
	Language string                     `json:"language,omitempty"`
	Duration float64                    `json:"duration,omitempty"`
	Words    []TranscriptionWord        `json:"words,omitempty"`
	Segments []TranscriptionSegment     `json:"segments,omitempty"`
	Usage    *TranscriptionUsage        `json:"usage,omitempty"`
	Extra    map[string]json.RawMessage `json:"-"`
}

// "task" is deliberately absent: the vendor emits it, dorang has no use for it,
// and listing it here would delete it from the answer rather than carry it.
var transcriptionResponseKnown = knownKeys("text", "language", "duration", "words",
	"segments", "usage")

// MarshalJSON implements [encoding/json.Marshaler].
func (r TranscriptionResponse) MarshalJSON() ([]byte, error) {
	type alias TranscriptionResponse
	return marshalWithExtra(alias(r), r.Extra, transcriptionResponseKnown)
}

// UnmarshalJSON implements [encoding/json.Unmarshaler].
func (r *TranscriptionResponse) UnmarshalJSON(b []byte) error {
	type alias TranscriptionResponse
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	extra, err := splitExtra(b, transcriptionResponseKnown)
	if err != nil {
		return err
	}
	*r = TranscriptionResponse(a)
	r.Extra = extra
	return nil
}

// TranscriptionWord is one word-level timing.
type TranscriptionWord struct {
	Word  string  `json:"word"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

// TranscriptionSegment is one timed segment.
type TranscriptionSegment struct {
	ID    int     `json:"id"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`

	Extra map[string]json.RawMessage `json:"-"`
}

var transcriptionSegmentKnown = knownKeys("id", "start", "end", "text")

// MarshalJSON implements [encoding/json.Marshaler].
func (s TranscriptionSegment) MarshalJSON() ([]byte, error) {
	type alias TranscriptionSegment
	return marshalWithExtra(alias(s), s.Extra, transcriptionSegmentKnown)
}

// UnmarshalJSON implements [encoding/json.Unmarshaler].
func (s *TranscriptionSegment) UnmarshalJSON(b []byte) error {
	type alias TranscriptionSegment
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	extra, err := splitExtra(b, transcriptionSegmentKnown)
	if err != nil {
		return err
	}
	*s = TranscriptionSegment(a)
	s.Extra = extra
	return nil
}

// TranscriptionUsage is the audio surface's own usage block.
//
// It is a third spelling of the same concept: tokens for a token-billed model,
// seconds for a duration-billed one. Both are read; a duration is not a token
// count and is never folded into one (DESIGN §10.7's rule that a mis-mapped
// count produces a wrong invoice rather than an error).
type TranscriptionUsage struct {
	Type         string  `json:"type,omitempty"`
	InputTokens  int     `json:"input_tokens,omitempty"`
	OutputTokens int     `json:"output_tokens,omitempty"`
	TotalTokens  int     `json:"total_tokens,omitempty"`
	Seconds      float64 `json:"seconds,omitempty"`
}

// DecodeTranscriptionResponse parses a transcription answer.
//
// mediaType is what the backend labelled the body. A non-JSON response format
// (text, srt, vtt) has no fields to read, so the bytes are carried verbatim and
// the text is filled from them, which keeps metering and the ledger seeing the
// same thing on every path.
func DecodeTranscriptionResponse(b []byte, mediaType string) (*canonical.TranscriptionResponse, error) {
	if !isJSONBody(b, mediaType) {
		return &canonical.TranscriptionResponse{
			Text: string(b),
			Raw:  &canonical.Binary{MediaType: mediaType, Data: append([]byte(nil), b...)},
		}, nil
	}
	var w TranscriptionResponse
	if err := json.Unmarshal(b, &w); err != nil {
		return nil, err
	}
	out := &canonical.TranscriptionResponse{
		Text:     w.Text,
		Language: w.Language,
		Duration: w.Duration,
		Extra:    w.Extra,
	}
	for i := range w.Words {
		out.Words = append(out.Words, canonical.TranscriptionWord(w.Words[i]))
	}
	for i := range w.Segments {
		s := &w.Segments[i]
		out.Segments = append(out.Segments, canonical.TranscriptionSegment{
			ID: s.ID, Start: s.Start, End: s.End, Text: s.Text, Extra: s.Extra,
		})
	}
	if w.Usage != nil {
		u := &canonical.Usage{
			InputTokens:  w.Usage.InputTokens,
			OutputTokens: w.Usage.OutputTokens,
		}
		if u.InputTokens == 0 && u.OutputTokens == 0 && w.Usage.TotalTokens > 0 {
			u.InputTokens = w.Usage.TotalTokens
		}
		out.Usage = u
	}
	return out, nil
}

// MarshalTranscriptionResponse encodes a neutral transcript.
//
// A response carried raw is re-emitted verbatim with its own media type: a
// gateway that re-renders an SRT file from parsed fields changes the cue
// numbering, and no client asked it to.
func MarshalTranscriptionResponse(r *canonical.TranscriptionResponse) ([]byte, string, error) {
	if r == nil {
		return nil, "", errorString("openai: nil transcription response")
	}
	if r.Raw != nil {
		mt := r.Raw.MediaType
		if mt == "" {
			mt = "text/plain"
		}
		return r.Raw.Data, mt, nil
	}
	w := TranscriptionResponse{
		Text:     r.Text,
		Language: r.Language,
		Duration: r.Duration,
		Extra:    r.Extra,
	}
	for i := range r.Words {
		w.Words = append(w.Words, TranscriptionWord(r.Words[i]))
	}
	for i := range r.Segments {
		s := &r.Segments[i]
		w.Segments = append(w.Segments, TranscriptionSegment{
			ID: s.ID, Start: s.Start, End: s.End, Text: s.Text, Extra: s.Extra,
		})
	}
	if r.Usage != nil {
		w.Usage = &TranscriptionUsage{
			Type:         "tokens",
			InputTokens:  r.Usage.InputTokens,
			OutputTokens: r.Usage.OutputTokens,
			TotalTokens:  r.Usage.TotalTokens(),
		}
	}
	b, err := Marshal(w)
	if err != nil {
		return nil, "", err
	}
	return b, "application/json", nil
}

// isJSONBody decides whether a transcription body is the JSON form.
//
// The media type is trusted when it says JSON and when it clearly says
// something else; when the backend labelled it nothing useful — which
// self-hosted engines do — the first non-space byte decides, because a JSON
// object starts with '{' and none of the subtitle formats do.
func isJSONBody(b []byte, mediaType string) bool {
	switch {
	case hasPrefixFold(mediaType, "application/json"), hasPrefixFold(mediaType, "text/json"):
		return true
	case mediaType != "" && !hasPrefixFold(mediaType, "application/octet-stream"):
		return false
	}
	b = trimSpace(b)
	return len(b) > 0 && b[0] == '{'
}

func hasPrefixFold(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != prefix[i] {
			return false
		}
	}
	return true
}

// formatFloat renders a form value without an exponent or trailing zeroes.
func formatFloat(v float64) string {
	b, err := Marshal(v)
	if err != nil {
		return "0"
	}
	return string(b)
}
