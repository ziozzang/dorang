package server

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/ziozzang/dorang/internal/canonical"
)

// Multipart request bodies.
//
// Three T1 inference routes take one — audio transcription, audio translation,
// and image edits and variations. They are parsed here, in the gate, rather
// than in each handler, for one reason: the authorization gate has to know the
// model before it can check the key's allow-list, and on these routes the model
// is a form FIELD. A handler that parsed the form itself would leave the gate
// authorizing a request with no model at all, which is exactly the shape of
// W10's allow-list bypass, one encoding removed.
//
// The whole body has already been read under max_body_bytes by the time this
// runs, so a file larger than the cap has already been refused with 413 and
// nothing here is an unbounded copy. What this adds on top is a bound on the
// NUMBER of parts, because a body that is entirely part headers is small on the
// wire and large in a map.

// maxFormParts bounds the parts in one multipart body. The busiest of these
// routes sends a file, a mask and about ten fields; anything past this is not a
// client dorang has to serve.
const maxFormParts = 64

// maxFormFieldBytes bounds one non-file field. Prompts on these surfaces are
// sentences, not documents, and an unbounded field is how a small body becomes
// a large map.
const maxFormFieldBytes = 1 << 20

// parseMultipart turns an already-read multipart body into a neutral form.
func parseMultipart(r *http.Request, body []byte) (*canonical.Form, *Error) {
	ct := r.Header.Get("Content-Type")
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil || !strings.HasPrefix(mt, "multipart/") {
		return nil, NewError(http.StatusBadRequest, TypeInvalidRequest,
			"expected a multipart/form-data body").WithCode(CodeInvalidRequest)
	}
	boundary := params["boundary"]
	if boundary == "" {
		return nil, NewError(http.StatusBadRequest, TypeInvalidRequest,
			"the multipart Content-Type carried no boundary").WithCode(CodeInvalidRequest)
	}

	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	form := &canonical.Form{Values: make(map[string][]string, 8)}
	for n := 0; ; n++ {
		if n >= maxFormParts {
			return nil, NewError(http.StatusBadRequest, TypeInvalidRequest,
				"the multipart body carried too many parts").WithCode(CodeInvalidRequest)
		}
		part, err := mr.NextPart()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, NewError(http.StatusBadRequest, TypeInvalidRequest,
				"the multipart body could not be parsed").WithCode(CodeInvalidRequest)
		}
		name := part.FormName()
		if name == "" {
			_ = part.Close()
			continue
		}
		if fn := part.FileName(); fn != "" {
			buf, rerr := readAllLimited(part, int64(len(body)))
			_ = part.Close()
			if rerr != nil {
				return nil, rerr
			}
			form.Files = append(form.Files, canonical.File{
				Field:     name,
				Name:      fn,
				MediaType: part.Header.Get("Content-Type"),
				Data:      buf,
			})
			continue
		}
		buf, rerr := readAllLimited(part, maxFormFieldBytes)
		_ = part.Close()
		if rerr != nil {
			return nil, rerr
		}
		// An exact map key: "Model" and "model" are different fields here, as
		// they are to every downstream parser (COMPATIBILITY 2.0).
		form.Values[name] = append(form.Values[name], string(buf))
	}
	return form, nil
}

// readAllLimited reads at most limit bytes, refusing rather than truncating.
func readAllLimited(r io.Reader, limit int64) ([]byte, *Error) {
	if limit < 0 {
		limit = 0
	}
	buf := make([]byte, 0, 512)
	tmp := make([]byte, 32<<10)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			if int64(len(buf))+int64(n) > limit {
				return nil, tooLarge(limit)
			}
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return buf, nil
			}
			return nil, NewError(http.StatusBadRequest, TypeInvalidRequest,
				"the multipart body could not be read").WithCode(CodeInvalidRequest)
		}
	}
}
