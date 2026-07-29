package canonical

import "encoding/json"

// File is an uploaded binary part.
//
// Audio and image endpoints take multipart bodies, and a multipart part is
// three things — a field name, a filename, and bytes — none of which survive a
// JSON representation. The bytes are held whole: the request body was already
// read under the server's size cap before this type is built, so there is no
// second, unbounded copy hiding here.
type File struct {
	// Field is the multipart form field the part arrived on ("file", "image",
	// "mask"). It is kept because an endpoint that takes two files distinguishes
	// them only by field name.
	Field string
	// Name is the client-supplied filename. Backends read it to infer a format,
	// so it is forwarded verbatim rather than regenerated.
	Name string
	// MediaType is the part's Content-Type, empty when the client sent none.
	MediaType string
	Data      []byte
}

// Len is the payload size in bytes.
func (f *File) Len() int {
	if f == nil {
		return 0
	}
	return len(f.Data)
}

// Binary is a non-JSON response payload: synthesized speech, a rendered image,
// a transcript in a subtitle format.
//
// It exists on the neutral type rather than being relayed around it, because a
// gateway that special-cases "this response is not JSON" ends up with a second
// response path that no conversion, metering or header rule applies to.
type Binary struct {
	MediaType string
	Data      []byte
}

// SpeechRequest is a protocol-neutral text-to-speech call.
type SpeechRequest struct {
	// Model is an opaque string (DESIGN §2.1).
	Model string
	Input string
	// Voice is a vendor-specific voice id, opaque for the same reason a model
	// name is.
	Voice string
	// Instructions steers delivery on models that accept it.
	Instructions string
	// Format is the container ("mp3", "opus", "aac", "flac", "wav", "pcm").
	// Empty means the backend default.
	Format string
	Speed  *float64
	// StreamFormat is the vendor's streaming container selector. dorang does not
	// stream this surface; the field is carried so a backend that requires it
	// sees what the caller sent.
	StreamFormat string
	Extra        map[string]json.RawMessage
}

// SpeechResponse is synthesized audio.
type SpeechResponse struct {
	Audio Binary
	Usage *Usage
}

// TranscriptionRequest is a protocol-neutral speech-to-text call.
//
// One type serves both transcription and translation, distinguished by
// Translate, because the two differ by one endpoint segment and one parameter
// the translation form does not accept.
type TranscriptionRequest struct {
	Model string
	File  File

	// Translate selects /v1/audio/translations semantics: the output is English
	// regardless of the input language, and Language is not accepted.
	Translate bool

	// Language is an ISO-639-1 hint for the INPUT language.
	Language string
	// Prompt biases the decoder toward a vocabulary.
	Prompt string
	// Format selects the response shape ("json", "text", "srt",
	// "verbose_json", "vtt"). Empty means "json".
	Format      string
	Temperature *float64
	// TimestampGranularities requests word- or segment-level timings. Only
	// meaningful with a verbose response format.
	TimestampGranularities []string
	Include                []string
	// Stream is the caller's streaming request. dorang does not stream this
	// surface and refuses rather than silently answering non-streaming.
	Stream bool

	// Extra carries every other form field, by wire name. Values are strings
	// because a multipart form has no types.
	Extra map[string]string
}

// TranscriptionResponse is a protocol-neutral transcript.
//
// A non-JSON response format (text, srt, vtt) has no fields to model, so it is
// carried in Raw and re-emitted verbatim. Text is still filled for those forms
// where it can be, so metering and the ledger see the same thing on every path.
type TranscriptionResponse struct {
	Text     string
	Language string
	// Duration is the audio length in seconds, 0 when the backend reported none.
	Duration float64
	Segments []TranscriptionSegment
	Words    []TranscriptionWord
	Usage    *Usage

	// UsageUnit is what the backend billed this transcript IN — "tokens" or
	// "duration" — and UsageSeconds the quantity when it is the latter. They are
	// separate from Duration, which is how long the audio was: a model can bill
	// a rounded minute for a 12.5-second clip, and only one of the two numbers
	// is on the invoice. Empty means the backend named no unit.
	//
	// These two are the VERBATIM wire values, re-emitted to the client exactly as
	// the vendor wrote them — including a unit word this build does not model.
	// The same two facts also ride on [Usage.Billed] and [Usage.AudioSeconds] in
	// canonical form, which is what internal/pricing and internal/meter read.
	//
	// A duration is never folded into a token count (DESIGN §10.7), and it is now
	// never folded into a WALL TIME either: `per_audio_second` prices this
	// quantity and `per_compute_second` prices the request's own duration. They
	// were one field until a ten-minute recording transcribed in eight seconds
	// was billed as eight seconds.
	UsageUnit    string
	UsageSeconds float64
	// UsageExtra carries the members of the upstream usage object that no
	// canonical counter names, above all its `input_token_details` breakdown —
	// text and audio tokens, priced apart by the models that report them.
	UsageExtra map[string]json.RawMessage

	// Raw is the verbatim body for a non-JSON response format, with the media
	// type the backend labelled it.
	Raw *Binary

	Extra map[string]json.RawMessage
}

// TranscriptionSegment is one timed segment of a verbose transcript.
//
// The acoustic diagnostics a verbose transcript carries are vendor-specific and
// numerous; they are kept raw rather than modelled, because dorang has no use
// for them and inventing a struct for each would date on the next model.
type TranscriptionSegment struct {
	ID    int
	Start float64
	End   float64
	Text  string
	Extra map[string]json.RawMessage
}

// TranscriptionWord is one word-level timing.
type TranscriptionWord struct {
	Word  string
	Start float64
	End   float64
}
