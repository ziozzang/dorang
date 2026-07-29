package canonical

import "encoding/json"

// ImageOp names which of the three image operations a request is.
type ImageOp string

// The image operations.
const (
	// ImageGenerate is /v1/images/generations: a prompt and no input image.
	ImageGenerate ImageOp = "generate"
	// ImageEdit is /v1/images/edits: one or more input images, an optional
	// mask, and a prompt.
	ImageEdit ImageOp = "edit"
	// ImageVariation is /v1/images/variations: one input image and no prompt.
	ImageVariation ImageOp = "variation"
)

// ImageRequest is a protocol-neutral image call.
//
// The three operations share nearly every parameter and differ in which of them
// are required, so they are one type with an Op rather than three types with a
// shared embedded struct — the encoders switch on Op exactly once.
type ImageRequest struct {
	Op ImageOp
	// Model is an opaque string (DESIGN §2.1).
	Model  string
	Prompt string

	N *int
	// Size is a vendor-specific dimension string ("1024x1024", "auto"). It is
	// NOT parsed: a gateway that splits it on 'x' to validate it rejects "auto"
	// and every future spelling.
	Size string
	// Quality, Style, Background and Moderation are vendor knobs carried
	// verbatim.
	Quality    string
	Style      string
	Background string
	Moderation string
	// Format selects url or b64_json delivery. Empty means the backend default,
	// which differs per model.
	Format string
	// OutputFormat is the image container ("png", "jpeg", "webp").
	OutputFormat      string
	OutputCompression *int
	User              string
	// PartialImages requests progressive renders on backends that stream them.
	PartialImages *int

	// Images are the input images of an edit or a variation. Several are
	// accepted because the edit surface takes a list.
	Images []File
	// Mask is the edit mask, nil when none was sent.
	Mask *File

	Stream bool

	// Extra carries every field dorang does not model, keyed by its wire name.
	Extra map[string]json.RawMessage
}

// ImageResponse is a protocol-neutral image answer.
type ImageResponse struct {
	Created int64
	Data    []ImageData
	// Usage is normalized inclusive like every other surface (DESIGN §10.7).
	Usage *Usage
	// Background, OutputFormat, Quality and Size echo what the backend actually
	// produced, which is not always what was asked for.
	Background   string
	OutputFormat string
	Quality      string
	Size         string
	Extra        map[string]json.RawMessage
	// UsageExtra carries the members of the upstream usage object, and of its
	// breakdown sub-object, that no canonical counter names — text_tokens and
	// image_tokens above all, which are the two halves of an image prompt and are
	// priced apart by every vendor that reports them. Same type and same reason
	// as [Response.UsageExtra]; dropping them because dorang cannot price them
	// deletes a line item from somebody's invoice.
	UsageExtra *UsageExtra
}

// ImageData is one rendered image.
type ImageData struct {
	// URL and B64JSON are alternatives; which one is filled follows the
	// request's Format and the backend's default.
	URL     string
	B64JSON string
	// RevisedPrompt is the prompt the backend actually rendered, when it
	// rewrote the caller's.
	RevisedPrompt string
}
