package canonical

import (
	"mime/multipart"
	"net/textproto"
	"strconv"
	"strings"
)

// Form is a parsed multipart/form-data body.
//
// Three of dorang's inference surfaces take one — audio transcription, audio
// translation, and image edits and variations — and a multipart body is not
// JSON, so it cannot go through the strict JSON filter that every other request
// path uses. It gets the equivalent guarantee a different way: field lookup is
// an exact map lookup, so "Model" and "model" are different fields here exactly
// as they are different keys to a hand-written scanner (COMPATIBILITY 2.0). The
// authorization gate and the adapter therefore resolve the same model from the
// same bytes, which is the property W10 is about.
//
// The whole body is already bounded by the server's request-size cap before a
// Form is built, so nothing here is an unbounded copy.
type Form struct {
	// Values are the non-file fields. A repeated field keeps every value, in
	// arrival order, because `timestamp_granularities[]` is sent that way.
	Values map[string][]string
	// Files are the file parts, in arrival order.
	Files []File
}

// Get returns the first value of a field, or "".
func (f *Form) Get(name string) string {
	if f == nil {
		return ""
	}
	if v := f.Values[name]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// All returns every value of a field.
//
// It accepts the bracketed spelling too: a form field sent as
// `timestamp_granularities[]` and one sent as `timestamp_granularities` are the
// same parameter, and which one a client sends depends on its HTTP library
// rather than on any intent.
func (f *Form) All(name string) []string {
	if f == nil {
		return nil
	}
	out := f.Values[name]
	if v := f.Values[name+"[]"]; len(v) > 0 {
		out = append(append([]string(nil), out...), v...)
	}
	return out
}

// Float reads a numeric field. ok is false when the field is absent or is not a
// number — a malformed number is not silently read as zero.
func (f *Form) Float(name string) (float64, bool) {
	s := f.Get(name)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// Int reads an integer field.
func (f *Form) Int(name string) (int, bool) {
	s := f.Get(name)
	if s == "" {
		return 0, false
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return v, true
}

// Bool reads a boolean field. Only the two exact spellings count; "1" and "yes"
// are not booleans and reading them as true is how a gateway enables something
// the caller never asked for.
func (f *Form) Bool(name string) (bool, bool) {
	switch f.Get(name) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

// File returns the first file part on a given field, or nil.
func (f *Form) File(field string) *File {
	if f == nil {
		return nil
	}
	for i := range f.Files {
		if f.Files[i].Field == field {
			return &f.Files[i]
		}
	}
	return nil
}

// FilesOn returns every file part on a field, accepting the bracketed spelling.
func (f *Form) FilesOn(field string) []File {
	if f == nil {
		return nil
	}
	var out []File
	for i := range f.Files {
		if n := f.Files[i].Field; n == field || n == field+"[]" {
			out = append(out, f.Files[i])
		}
	}
	return out
}

// Set replaces a field's values.
func (f *Form) Set(name, value string) {
	if f.Values == nil {
		f.Values = make(map[string][]string, 8)
	}
	f.Values[name] = []string{value}
}

// Add appends a value to a field.
func (f *Form) Add(name, value string) {
	if f.Values == nil {
		f.Values = make(map[string][]string, 8)
	}
	f.Values[name] = append(f.Values[name], value)
}

// Encode renders the form as a multipart body and returns it with the boundary
// it used.
//
// Field order is deterministic — the caller supplies it — because a golden
// byte comparison of an outgoing request is only meaningful if the bytes do not
// depend on map iteration. Files follow the named fields, which is the order
// every client library emits and the order a streaming server-side parser
// needs when a field describes the file that follows it.
func (f *Form) Encode(boundary string, order []string) ([]byte, error) {
	buf := &strings.Builder{}
	mw := multipart.NewWriter(buf)
	if boundary != "" {
		if err := mw.SetBoundary(boundary); err != nil {
			return nil, err
		}
	}
	written := make(map[string]struct{}, len(f.Values))
	write := func(name string) error {
		if _, done := written[name]; done {
			return nil
		}
		written[name] = struct{}{}
		for _, v := range f.Values[name] {
			if err := mw.WriteField(name, v); err != nil {
				return err
			}
		}
		return nil
	}
	for _, name := range order {
		if err := write(name); err != nil {
			return nil, err
		}
	}
	rest := make([]string, 0, len(f.Values))
	for name := range f.Values {
		if _, done := written[name]; !done {
			rest = append(rest, name)
		}
	}
	sortStrings(rest)
	for _, name := range rest {
		if err := write(name); err != nil {
			return nil, err
		}
	}
	for i := range f.Files {
		file := &f.Files[i]
		h := make(textproto.MIMEHeader, 2)
		h.Set("Content-Disposition", `form-data; name="`+escapeQuotes(file.Field)+
			`"; filename="`+escapeQuotes(file.Name)+`"`)
		ct := file.MediaType
		if ct == "" {
			ct = "application/octet-stream"
		}
		h.Set("Content-Type", ct)
		w, err := mw.CreatePart(h)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(file.Data); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

// escapeQuotes matches mime/multipart's own escaper, which is unexported there.
var quoteEscaper = strings.NewReplacer("\\", "\\\\", `"`, "\\\"")

func escapeQuotes(s string) string { return quoteEscaper.Replace(s) }

// sortStrings is an insertion sort. The slice is a handful of form field names;
// pulling in sort for it would cost more than it saves.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
