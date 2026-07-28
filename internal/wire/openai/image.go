package openai

import (
	"encoding/json"
	"strconv"

	"github.com/ziozzang/dorang/internal/canonical"
)

// The image surfaces: POST /v1/images/generations, /v1/images/edits and
// /v1/images/variations.
//
// Generation takes JSON; edits and variations take multipart, because they
// carry an image and possibly a mask. All three answer with the same object, so
// there is one response adapter and two request adapters.

// Image form field names.
const (
	FieldImage             = "image"
	FieldMask              = "mask"
	FieldN                 = "n"
	FieldSize              = "size"
	FieldQuality           = "quality"
	FieldStyle             = "style"
	FieldBackground        = "background"
	FieldModeration        = "moderation"
	FieldOutputFormat      = "output_format"
	FieldOutputCompression = "output_compression"
	FieldUser              = "user"
	FieldPartialImages     = "partial_images"
)

// ImageRequest is a JSON image-generation request.
type ImageRequest struct {
	Prompt string `json:"prompt"`
	Model  string `json:"model,omitempty"`

	N *int `json:"n,omitempty"`
	// Size is carried verbatim and never parsed. Splitting it on 'x' to
	// validate it rejects "auto" and every dimension a provider ships next.
	Size              string `json:"size,omitempty"`
	Quality           string `json:"quality,omitempty"`
	Style             string `json:"style,omitempty"`
	Background        string `json:"background,omitempty"`
	Moderation        string `json:"moderation,omitempty"`
	ResponseFormat    string `json:"response_format,omitempty"`
	OutputFormat      string `json:"output_format,omitempty"`
	OutputCompression *int   `json:"output_compression,omitempty"`
	PartialImages     *int   `json:"partial_images,omitempty"`
	User              string `json:"user,omitempty"`
	Stream            bool   `json:"stream,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

var imageRequestKnown = knownKeys("prompt", "model", "n", "size", "quality",
	"style", "background", "moderation", "response_format", "output_format",
	"output_compression", "partial_images", "user", "stream")

// MarshalJSON implements [encoding/json.Marshaler].
func (r ImageRequest) MarshalJSON() ([]byte, error) {
	type alias ImageRequest
	return marshalWithExtra(alias(r), r.Extra, imageRequestKnown)
}

// UnmarshalJSON implements [encoding/json.Unmarshaler] with case-SENSITIVE
// field matching (COMPATIBILITY 2.0).
func (r *ImageRequest) UnmarshalJSON(b []byte) error {
	type alias ImageRequest
	var a alias
	b = strictBytes(b, &a)
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	extra, err := splitExtra(b, imageRequestKnown)
	if err != nil {
		return err
	}
	*r = ImageRequest(a)
	r.Extra = extra
	return nil
}

// DecodeImageRequest parses a JSON generation request into the neutral form.
func DecodeImageRequest(b []byte) (*canonical.ImageRequest, error) {
	var w ImageRequest
	if err := strictUnmarshal(b, &w); err != nil {
		return nil, err
	}
	return &canonical.ImageRequest{
		Op:                canonical.ImageGenerate,
		Model:             w.Model,
		Prompt:            w.Prompt,
		N:                 w.N,
		Size:              w.Size,
		Quality:           w.Quality,
		Style:             w.Style,
		Background:        w.Background,
		Moderation:        w.Moderation,
		Format:            w.ResponseFormat,
		OutputFormat:      w.OutputFormat,
		OutputCompression: w.OutputCompression,
		PartialImages:     w.PartialImages,
		User:              w.User,
		Stream:            w.Stream,
		Extra:             w.Extra,
	}, nil
}

// MarshalImageRequest encodes a neutral generation request as JSON.
func MarshalImageRequest(req *canonical.ImageRequest, model string) ([]byte, error) {
	if req == nil {
		return nil, errNilRequest
	}
	w := ImageRequest{
		Prompt:            req.Prompt,
		Model:             req.Model,
		N:                 req.N,
		Size:              req.Size,
		Quality:           req.Quality,
		Style:             req.Style,
		Background:        req.Background,
		Moderation:        req.Moderation,
		ResponseFormat:    req.Format,
		OutputFormat:      req.OutputFormat,
		OutputCompression: req.OutputCompression,
		PartialImages:     req.PartialImages,
		User:              req.User,
		Stream:            req.Stream,
		Extra:             req.Extra,
	}
	if model != "" {
		w.Model = model
	}
	return Marshal(w)
}

// imageFormKnown is the set of form fields this adapter models.
var imageFormKnown = map[string]struct{}{
	FieldImage: {}, FieldImage + "[]": {}, FieldMask: {}, FieldModel: {},
	FieldPrompt: {}, FieldN: {}, FieldSize: {}, FieldQuality: {}, FieldStyle: {},
	FieldBackground: {}, FieldModeration: {}, FieldResponseFormat: {},
	FieldOutputFormat: {}, FieldOutputCompression: {}, FieldUser: {},
	FieldPartialImages: {}, FieldStream: {},
}

// DecodeImageForm converts a parsed multipart edit or variation body.
//
// Field lookup is an exact map lookup, so a field spelled "Model" is not this
// endpoint's model — the same rule the JSON paths get from
// [canonical.StrictBytes] (COMPATIBILITY 2.0).
func DecodeImageForm(f *canonical.Form, op canonical.ImageOp) (*canonical.ImageRequest, error) {
	if f == nil {
		return nil, errNilRequest
	}
	images := f.FilesOn(FieldImage)
	if len(images) == 0 {
		return nil, errorString("openai: the multipart body carried no image part")
	}
	out := &canonical.ImageRequest{
		Op:           op,
		Model:        f.Get(FieldModel),
		Size:         f.Get(FieldSize),
		Quality:      f.Get(FieldQuality),
		Style:        f.Get(FieldStyle),
		Background:   f.Get(FieldBackground),
		Moderation:   f.Get(FieldModeration),
		Format:       f.Get(FieldResponseFormat),
		OutputFormat: f.Get(FieldOutputFormat),
		User:         f.Get(FieldUser),
		Images:       images,
	}
	if op != canonical.ImageVariation {
		// The variation surface takes no prompt. Forwarding one produces a 400
		// the caller has never seen.
		out.Prompt = f.Get(FieldPrompt)
	}
	if m := f.File(FieldMask); m != nil {
		out.Mask = m
	}
	if v, ok := f.Int(FieldN); ok {
		out.N = &v
	}
	if v, ok := f.Int(FieldOutputCompression); ok {
		out.OutputCompression = &v
	}
	if v, ok := f.Int(FieldPartialImages); ok {
		out.PartialImages = &v
	}
	if v, ok := f.Bool(FieldStream); ok {
		out.Stream = v
	}
	for name, values := range f.Values {
		if _, known := imageFormKnown[name]; known || len(values) == 0 {
			continue
		}
		if out.Extra == nil {
			out.Extra = make(map[string]json.RawMessage, 4)
		}
		enc, err := Marshal(values[0])
		if err != nil {
			return nil, err
		}
		out.Extra[name] = enc
	}
	return out, nil
}

// EncodeImageForm renders a neutral edit or variation request as multipart.
func EncodeImageForm(req *canonical.ImageRequest, model, boundary string) ([]byte, error) {
	if req == nil {
		return nil, errNilRequest
	}
	f := &canonical.Form{}
	name := req.Model
	if model != "" {
		name = model
	}
	if name != "" {
		f.Set(FieldModel, name)
	}
	if req.Prompt != "" && req.Op != canonical.ImageVariation {
		f.Set(FieldPrompt, req.Prompt)
	}
	setIfNotEmpty(f, FieldSize, req.Size)
	setIfNotEmpty(f, FieldQuality, req.Quality)
	setIfNotEmpty(f, FieldStyle, req.Style)
	setIfNotEmpty(f, FieldBackground, req.Background)
	setIfNotEmpty(f, FieldModeration, req.Moderation)
	setIfNotEmpty(f, FieldResponseFormat, req.Format)
	setIfNotEmpty(f, FieldOutputFormat, req.OutputFormat)
	setIfNotEmpty(f, FieldUser, req.User)
	if req.N != nil {
		f.Set(FieldN, itoa(*req.N))
	}
	if req.OutputCompression != nil {
		f.Set(FieldOutputCompression, itoa(*req.OutputCompression))
	}
	for k, v := range req.Extra {
		if _, known := imageFormKnown[k]; known {
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			s = string(v)
		}
		f.Set(k, s)
	}
	for i := range req.Images {
		file := req.Images[i]
		file.Field = FieldImage
		if len(req.Images) > 1 {
			file.Field = FieldImage + "[]"
		}
		if file.Name == "" {
			file.Name = "image"
		}
		f.Files = append(f.Files, file)
	}
	if req.Mask != nil {
		mask := *req.Mask
		mask.Field = FieldMask
		if mask.Name == "" {
			mask.Name = "mask"
		}
		f.Files = append(f.Files, mask)
	}
	return f.Encode(boundary, []string{
		FieldModel, FieldPrompt, FieldN, FieldSize, FieldQuality, FieldStyle,
		FieldBackground, FieldModeration, FieldResponseFormat, FieldOutputFormat,
		FieldOutputCompression, FieldUser,
	})
}

// itoa renders an integer form value.
func itoa(v int) string { return strconv.Itoa(v) }

func setIfNotEmpty(f *canonical.Form, name, value string) {
	if value != "" {
		f.Set(name, value)
	}
}

// ---------------------------------------------------------------------------
// Response
// ---------------------------------------------------------------------------

// ImageResponse is the answer of all three image operations.
type ImageResponse struct {
	Created int64       `json:"created"`
	Data    []ImageData `json:"data"`

	Background   string      `json:"background,omitempty"`
	OutputFormat string      `json:"output_format,omitempty"`
	Quality      string      `json:"quality,omitempty"`
	Size         string      `json:"size,omitempty"`
	Usage        *ImageUsage `json:"usage,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

var imageResponseKnown = knownKeys("created", "data", "background",
	"output_format", "quality", "size", "usage")

// MarshalJSON implements [encoding/json.Marshaler].
func (r ImageResponse) MarshalJSON() ([]byte, error) {
	type alias ImageResponse
	return marshalWithExtra(alias(r), r.Extra, imageResponseKnown)
}

// UnmarshalJSON implements [encoding/json.Unmarshaler].
func (r *ImageResponse) UnmarshalJSON(b []byte) error {
	type alias ImageResponse
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	extra, err := splitExtra(b, imageResponseKnown)
	if err != nil {
		return err
	}
	*r = ImageResponse(a)
	r.Extra = extra
	return nil
}

// ImageData is one rendered image.
type ImageData struct {
	URL           string `json:"url,omitempty"`
	B64JSON       string `json:"b64_json,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}

// ImageUsage is the image surface's usage block. Its input detail splits text
// from image tokens, and both are already inside input_tokens — the same
// inclusive rule as everywhere else (DESIGN §10.7).
type ImageUsage struct {
	InputTokens        int                     `json:"input_tokens"`
	OutputTokens       int                     `json:"output_tokens"`
	TotalTokens        int                     `json:"total_tokens"`
	InputTokensDetails *ImageInputTokenDetails `json:"input_tokens_details,omitempty"`
}

// ImageInputTokenDetails breaks the prompt count down.
type ImageInputTokenDetails struct {
	TextTokens  int `json:"text_tokens"`
	ImageTokens int `json:"image_tokens"`
}

// DecodeImageResponse parses an image answer into the neutral form.
func DecodeImageResponse(b []byte) (*canonical.ImageResponse, error) {
	var w ImageResponse
	if err := json.Unmarshal(b, &w); err != nil {
		return nil, err
	}
	out := &canonical.ImageResponse{
		Created:      w.Created,
		Background:   w.Background,
		OutputFormat: w.OutputFormat,
		Quality:      w.Quality,
		Size:         w.Size,
		Extra:        w.Extra,
	}
	for i := range w.Data {
		out.Data = append(out.Data, canonical.ImageData(w.Data[i]))
	}
	if w.Usage != nil {
		out.Usage = &canonical.Usage{
			InputTokens:  w.Usage.InputTokens,
			OutputTokens: w.Usage.OutputTokens,
		}
	}
	return out, nil
}

// MarshalImageResponse encodes a neutral image answer.
func MarshalImageResponse(r *canonical.ImageResponse) ([]byte, error) {
	if r == nil {
		return nil, errorString("openai: nil image response")
	}
	w := ImageResponse{
		Created:      r.Created,
		Background:   r.Background,
		OutputFormat: r.OutputFormat,
		Quality:      r.Quality,
		Size:         r.Size,
		Extra:        r.Extra,
	}
	w.Data = make([]ImageData, len(r.Data))
	for i := range r.Data {
		w.Data[i] = ImageData(r.Data[i])
	}
	if r.Usage != nil {
		w.Usage = &ImageUsage{
			InputTokens:  r.Usage.InputTokens,
			OutputTokens: r.Usage.OutputTokens,
			TotalTokens:  r.Usage.TotalTokens(),
		}
	}
	return Marshal(w)
}
