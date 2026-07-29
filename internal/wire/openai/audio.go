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

// ErrNotSpeechAudio is an upstream 200 on the speech route whose body is not an
// audio container.
//
// It is the same defect class as [ErrNotAResponse] on a surface where the JSON
// rule cannot be applied at all, and it is the worst-placed member of the class
// in this build. Speech is a pure BYTE RELAY: the upstream's bytes are handed to
// the client unexamined, and dorang stamps its OWN content type on them —
// derived from the container the caller asked for, not from what the backend
// said, because a self-hosted engine routinely mislabels real audio. So a vendor
// error envelope came back out as `{"code":500,"msg":"404 NOT_FOUND"}` under
// `Content-Type: audio/mpeg`, over a 200. Nothing downstream can tell that from
// audio until a player, a browser or an ffmpeg in someone's pipeline fails on it,
// a long way from the gateway that produced it.
var ErrNotSpeechAudio = errorString("openai: the speech route answered 200 with a body that is not audio")

// IsSpeechAudio reports whether a speech answer is an audio container.
//
// # Why the JSON rule does not apply here
//
// Every other gate in this package asks whether a decoded object carries one of
// the members its family's answers have. A speech answer has no members: it is
// an MP3, an Ogg page, a FLAC frame or raw PCM. There is no discriminator to
// read, no payload key to find present, and unmarshalling it is not a thing that
// can be attempted. The pair of facts that IS available is what the upstream
// labelled the body and whether there are any bytes in it, so that is the test.
//
// It refuses on exactly three grounds:
//
//  1. The body is empty. No audio container is zero bytes — every one of them
//     has a header — and a client handed nothing has nothing to play. This is
//     the deliberate opposite of the transcription rule: an empty TRANSCRIPT is
//     a correct transcript of silence, an empty RECORDING is not a recording of
//     silence.
//  2. The upstream labelled it as one of the text types in
//     [speechNotAudioTypes]. No speech route answers any of them.
//  3. The bytes are a complete JSON object, whatever the label says. This is the
//     clause that does the work, because the label is the part that cannot be
//     trusted: a misrouted host answers `{"code":500,…}` under
//     `application/json` when it sets a type at all and under nothing when it
//     does not, and Go's own sniffer then calls it `text/plain`.
//
// What it does NOT do is require the label to say `audio/*`. That would break
// every self-hosted deployment, because answering `application/octet-stream` for
// real audio is the ordinary case there — it is the reason [SpeechMediaType]
// exists at all. An unhelpful or unrecognized label is therefore read as no
// information rather than as a verdict, and the bytes are asked instead. Raw PCM
// whose first byte happens to be '{' is not also balanced, valid JSON to its
// last byte, so clause 3 costs nothing real.
//
// A `stream_format: sse` body passes: `text/event-stream` is deliberately not in
// [speechNotAudioTypes], and an SSE frame sequence is not a JSON object.
func IsSpeechAudio(b []byte, mediaType string) bool {
	if len(trimSpace(b)) == 0 {
		return false
	}
	for _, t := range speechNotAudioTypes {
		if hasPrefixFold(mediaType, t) {
			return false
		}
	}
	return !isJSONObject(b)
}

// speechNotAudioTypes are the response types a speech route never answers.
//
// It is an enumeration rather than a `text/*` wildcard on purpose. `audio/*`,
// `application/octet-stream`, `text/event-stream` and every label dorang has not
// heard of are all ACCEPTED, because refusing a container this build does not
// know the name of is a worse failure than relaying an oddly-labelled error: one
// breaks a working deployment, the other is caught by the byte test below it.
var speechNotAudioTypes = []string{
	"application/json", "text/json", "text/plain", "text/html", "text/xml",
	"application/xml",
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
// It is a third spelling of the same concept, and it carries two different
// BILLING UNITS: tokens for a token-billed model, seconds for a duration-billed
// one, discriminated by `type`. Both are read; a duration is not a token count
// and is never folded into one (DESIGN §10.7's rule that a mis-mapped count
// produces a wrong invoice rather than an error).
//
// "Both are read" is what this comment said while neither `type` nor `seconds`
// was read on the way in and the encoder wrote `"type":"tokens"` on the way out
// unconditionally. A duration-billed transcript therefore reached the client
// relabelled as token-billed, with a token count of zero and the billed duration
// deleted — a bill stating a unit it was not charged in.
//
// Extra is the fourth spelling this surface has to survive: its breakdown object
// is `input_token_details` — singular `token` — splitting an audio prompt into
// text and audio tokens, which are priced apart. dorang counts neither, so they
// ride rather than being deleted.
type TranscriptionUsage struct {
	Type         string  `json:"type,omitempty"`
	InputTokens  int     `json:"input_tokens,omitempty"`
	OutputTokens int     `json:"output_tokens,omitempty"`
	TotalTokens  int     `json:"total_tokens,omitempty"`
	Seconds      float64 `json:"seconds,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

var transcriptionUsageKnown = knownKeys("type", "input_tokens", "output_tokens",
	"total_tokens", "seconds")

// MarshalJSON implements [encoding/json.Marshaler].
func (u TranscriptionUsage) MarshalJSON() ([]byte, error) {
	type alias TranscriptionUsage
	return marshalWithExtra(alias(u), u.Extra, transcriptionUsageKnown)
}

// UnmarshalJSON implements [encoding/json.Unmarshaler].
func (u *TranscriptionUsage) UnmarshalJSON(b []byte) error {
	type alias TranscriptionUsage
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	extra, err := splitExtraFold(b, transcriptionUsageKnown)
	if err != nil {
		return err
	}
	*u = TranscriptionUsage(a)
	u.Extra = extra
	return nil
}

// ErrNotATranscriptionResponse is a JSON object that parsed cleanly and is not a
// transcript.
//
// The audio surface reached the client with the vendor's error envelope as the
// ENTIRE answer: `text` is empty so it is omitted from the re-serialized object,
// leaving nothing but the upstream's `code`/`msg`/`success` carried out through
// Extra. A caller reading `.text` off that got the empty string and no error at
// all.
var ErrNotATranscriptionResponse = errorString("openai: the body is a JSON object but not a transcription response")

// transcriptionPayload is the member set that makes a JSON body a transcript.
//
// `task` is the discriminator — the vendor emits "transcribe" or "translate" on
// the verbose form — and the rest are the payload. Accepting on ANY of them is
// the same either-ground rule the chat gate states: a backend that omits `task`
// still sends `text`, and a verbose answer whose text member the vendor happens
// to omit still sends `segments`.
var transcriptionPayload = []string{"text", "segments", "words", "usage", "task"}

// IsTranscriptionResponse reports whether a body is a transcript of this family.
//
// It takes BYTES rather than the decoded struct, unlike the other gates in this
// package, and the reason is specific: this surface's payload member is `text`,
// a plain string with no absence marker on the struct. `{"text":""}` — the
// correct transcript of silence — and a body with no `text` at all decode to the
// identical value, so the question can only be answered before the decode.
//
// A body that is not a JSON object is accepted unconditionally. That is not a
// gap: see [DecodeTranscriptionResponse] for why the raw form has no shape to
// test, and why an empty one is a correct answer rather than a missing one.
func IsTranscriptionResponse(b []byte) bool {
	if !isJSONObject(b) {
		return true
	}
	return hasAnyMember(b, transcriptionPayload...)
}

// DecodeTranscriptionResponse parses a transcription answer.
//
// mediaType is what the backend labelled the body. A non-JSON response format
// (text, srt, vtt) has no fields to read, so the bytes are carried verbatim and
// the text is filled from them, which keeps metering and the ledger seeing the
// same thing on every path.
//
// It returns [ErrNotATranscriptionResponse] for a JSON object that is not a
// transcript, and the check runs BEFORE the JSON/raw split rather than inside
// the JSON branch. That placement is the whole fix on this surface, because the
// raw branch is a byte relay and is where a misrouted upstream actually lands:
// [isJSONBody] trusts a media type that clearly says something else, so a vendor
// error envelope answered under a sniffed `text/plain` — which is what a body
// with no Content-Type at all becomes — was handed to the client verbatim AS THE
// TRANSCRIPT. Checking first covers both branches with one rule.
//
// What the raw branch is deliberately NOT checked for is emptiness. Silence
// transcribes to an empty string, so zero bytes is a correct answer here — the
// opposite of the speech surface, where zero bytes is not a playable container.
// Nothing else about a subtitle file is decidable: dorang cannot tell a wrong
// transcript from a right one, and only refuses the one case it can name, which
// is a body that is a whole JSON object carrying none of a transcript's members.
func DecodeTranscriptionResponse(b []byte, mediaType string) (*canonical.TranscriptionResponse, error) {
	if !IsTranscriptionResponse(b) {
		return nil, ErrNotATranscriptionResponse
	}
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
		// Reported, like every other decoder in this package and unlike this one
		// until now. A backend that says a transcript cost zero tokens has
		// MEASURED; one that sends no usage object has not, and an encoder with
		// only the integer to look at cannot tell the two apart (see
		// [canonical.UsageField]). That distinction was the whole of the
		// cached_tokens defect closed on the chat surface.
		u := &canonical.Usage{
			InputTokens:  w.Usage.InputTokens,
			OutputTokens: w.Usage.OutputTokens,
			Reported:     canonical.UsageInput | canonical.UsageOutput,
			// The two halves of §10.7's "a billing unit is never converted", on
			// the type that reaches pricing and metering. Until they were here
			// the recording's length reached the neutral transcript and went no
			// further, and a per-second rate was applied to the request's WALL
			// TIME instead: a ten-minute recording transcribed in eight seconds
			// was charged as eight seconds.
			AudioSeconds: w.Usage.Seconds,
			Billed:       canonical.ParseBilledUnit(w.Usage.Type),
		}
		if u.InputTokens == 0 && u.OutputTokens == 0 && w.Usage.TotalTokens > 0 {
			// A transcript has no completion half on a duration-billed model and
			// on several token-billed ones, so a stated total IS the prompt count
			// — the same reading internal/backend's scanRelayUsage applies to an
			// embedding.
			u.InputTokens = w.Usage.TotalTokens
		}
		out.Usage = u
		// The verbatim wire spelling, kept beside the typed pair above because
		// they answer different questions: these two are re-emitted to the client
		// exactly as the vendor wrote them, including a unit word this build does
		// not model, while the typed pair is what dorang prices and meters.
		out.UsageUnit = w.Usage.Type
		out.UsageSeconds = w.Usage.Seconds
		out.UsageExtra = w.Usage.Extra
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
			// The unit the backend named, never one this encoder chose. "tokens"
			// is the fallback because it is what a token count means and what
			// every model that reports one calls it — but a model that billed by
			// the second said so, and overwriting that told the client it was
			// charged in a unit nobody charged it in.
			Type:         r.UsageUnit,
			InputTokens:  r.Usage.InputTokens,
			OutputTokens: r.Usage.OutputTokens,
			TotalTokens:  r.Usage.TotalTokens(),
			Seconds:      r.UsageSeconds,
			Extra:        r.UsageExtra,
		}
		if w.Usage.Type == "" {
			w.Usage.Type = "tokens"
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
