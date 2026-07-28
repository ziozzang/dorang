package batch

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/ziozzang/dorang/internal/prefix"
)

// validateConfig is what one validation pass needs. It is separated from
// [Config] so that upload-time validation (which knows no endpoint) and
// create-time validation (which requires every row to match the batch's
// endpoint) are the same code path with one field different.
type validateConfig struct {
	// endpoint, when set, requires every row's url to equal it.
	endpoint     string
	endpoints    map[string]bool
	maxRows      int
	maxRowBytes  int
	maxCustomID  int
	maxErrors    int
	groupSegment int
	models       ModelResolver
}

// validateInput scans a JSONL batch input file.
//
// It returns a non-nil *ValidationErrors when the content is invalid, and a
// plain error only when the reader itself failed. collect, if given, is called
// once per valid row in file order and is how the caller materializes rows
// without a second pass.
//
// Scanning does not stop at the first bad line. A caller fixing a 50 000-line
// file wants the list, not a sequence of single-error round trips — which is
// also why OpenAI's batch object carries an errors *list*.
func validateInput(rd io.Reader, vc validateConfig, collect func(*RowRecord)) (*ValidationErrors, error) {
	ve := &ValidationErrors{}
	sc := &lineScanner{br: bufio.NewReaderSize(rd, 64<<10), max: vc.maxRowBytes}
	seen := make(map[string]int)
	rows := 0
	keepGoing := true

	for keepGoing {
		ln, err := sc.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("batch: reading input file: %w", err)
		}
		if len(bytes.TrimSpace(ln.data)) == 0 {
			continue
		}
		if ln.tooLong {
			keepGoing = ve.add(vc.maxErrors, &ValidationError{
				Line: ln.num, Code: CodeLineTooLarge,
				Message: fmt.Sprintf("line is longer than the %d byte limit", vc.maxRowBytes),
			})
			continue
		}

		rows++
		if vc.maxRows > 0 && rows > vc.maxRows {
			ve.add(vc.maxErrors, &ValidationError{
				Line: ln.num, Code: CodeTooManyRequests,
				Message: fmt.Sprintf("input file has more than the %d request limit", vc.maxRows),
			})
			break
		}

		var in InputRow
		dec := json.NewDecoder(bytes.NewReader(ln.data))
		if err := dec.Decode(&in); err != nil {
			keepGoing = ve.add(vc.maxErrors, &ValidationError{
				Line: ln.num, Code: CodeInvalidJSON,
				Message: "not a valid JSON object: " + err.Error(),
			})
			continue
		}
		if dec.More() {
			keepGoing = ve.add(vc.maxErrors, &ValidationError{
				Line: ln.num, Code: CodeInvalidJSON,
				Message: "line carries more than one JSON value",
			})
			continue
		}

		if bad := validateRow(&in, ln, vc, seen); bad != nil {
			keepGoing = ve.add(vc.maxErrors, bad)
			continue
		}
		seen[in.CustomID] = ln.num

		if collect != nil {
			model := modelOf(in.Body)
			collect(&RowRecord{
				CustomID: in.CustomID,
				Seq:      rows - 1,
				// Hashed over the BODY, not over the line. The line begins with
				// the custom_id, which is unique by definition, so hashing the
				// line would give every row a different group and turn the
				// grouping into a no-op that still looked implemented.
				PrefixHash: groupHash(model, in.Body, vc.groupSegment),
				Model:      model,
				URL:        in.URL,
				Offset:     ln.off,
				Length:     len(ln.data),
				Status:     RowQueued,
			})
		}
	}

	if ve.Len() == 0 && rows == 0 {
		ve.add(vc.maxErrors, &ValidationError{
			Code: CodeEmptyFile, Message: "input file contains no requests",
		})
	}
	if ve.Len() > 0 {
		return ve, nil
	}
	return nil, nil
}

// validateRow applies every per-row rule, returning the first failure. One
// failure per line is enough: the second complaint about the same line is
// noise, and the caller is going to re-edit that line either way.
func validateRow(in *InputRow, ln *rawLine, vc validateConfig, seen map[string]int) *ValidationError {
	switch {
	case in.CustomID == "":
		return &ValidationError{Line: ln.num, Code: CodeMissingCustomID, Param: "custom_id",
			Message: "custom_id is required and must be a non-empty string"}
	case vc.maxCustomID > 0 && utf8.RuneCountInString(in.CustomID) > vc.maxCustomID:
		return &ValidationError{Line: ln.num, Code: CodeCustomIDTooLong, Param: "custom_id",
			Message: fmt.Sprintf("custom_id is longer than %d characters", vc.maxCustomID)}
	}
	if first, dup := seen[in.CustomID]; dup {
		return &ValidationError{Line: ln.num, Code: CodeDuplicateID, Param: "custom_id",
			Message: fmt.Sprintf("custom_id %q is already used on line %d", in.CustomID, first)}
	}
	if in.Method != "POST" {
		return &ValidationError{Line: ln.num, Code: CodeInvalidMethod, Param: "method",
			Message: fmt.Sprintf("method must be POST, got %q", in.Method)}
	}
	if !vc.endpoints[in.URL] {
		return &ValidationError{Line: ln.num, Code: CodeInvalidURL, Param: "url",
			Message: fmt.Sprintf("url %q is not a batch-capable endpoint", in.URL)}
	}
	if vc.endpoint != "" && in.URL != vc.endpoint {
		return &ValidationError{Line: ln.num, Code: CodeEndpointMismatch, Param: "url",
			Message: fmt.Sprintf("url %q does not match the batch endpoint %q", in.URL, vc.endpoint)}
	}
	if len(bytes.TrimSpace(in.Body)) == 0 {
		return &ValidationError{Line: ln.num, Code: CodeMissingBody, Param: "body",
			Message: "body is required"}
	}
	var probe struct {
		Model *string `json:"model"`
	}
	if err := json.Unmarshal(in.Body, &probe); err != nil {
		return &ValidationError{Line: ln.num, Code: CodeInvalidBody, Param: "body",
			Message: "body is not a JSON object: " + err.Error()}
	}
	if probe.Model == nil || *probe.Model == "" {
		return &ValidationError{Line: ln.num, Code: CodeMissingModel, Param: "body.model",
			Message: "body.model is required"}
	}
	// The model name is an opaque string (DESIGN §2.1). It is handed to the
	// resolver whole; nothing here splits it on ':' or '/'.
	if vc.models != nil {
		if _, ok := vc.models.ResolveModel(*probe.Model); !ok {
			return &ValidationError{Line: ln.num, Code: CodeModelNotFound, Param: "body.model",
				Message: fmt.Sprintf("model %q is not served by any deployment", *probe.Model)}
		}
	}
	return nil
}

// modelOf extracts body.model without validating it. Callers only reach it
// after validateRow has passed.
func modelOf(body json.RawMessage) string {
	var probe struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &probe)
	return probe.Model
}

// groupHash is the scheduler's grouping key: hex of the shallowest digest of
// the prefix chain over the row's request body, seeded with the model name
// (DESIGN §7.4b).
//
// Three things about it are deliberate.
//
// It hashes the body rather than the JSONL line. The line starts with the
// custom_id, which every row is required to make unique, so a chain over the
// line diverges at its first segment for every row in the file and the grouping
// silently degenerates into no grouping at all.
//
// The base segment is [Config.GroupSegment], much smaller than
// prefix.DefaultBaseSegment. Routing uses 4 KiB because that is the granularity
// at which a backend's cache is worth chasing; grouping at 4 KiB would put every
// batch row shorter than 4 KiB — which is most of them — in a segment covering
// the entire body, so two rows differing only in their last sentence would hash
// differently and never group. The whole point is to group rows that share a
// long head, so the head has to be cut smaller than the rows are.
//
// And the digest is used as a sort key, not as a proof. §7.4b's chain exists to
// make a cache-affinity claim honest, where a collision would send a request to
// a backend holding something else. Here a collision costs nothing but a
// slightly worse ordering, so the shallowest digest is exactly the right
// resolution.
func groupHash(model string, line []byte, segment int) string {
	d := prefix.Compute(model, line, segment)
	if len(d) == 0 {
		return ""
	}
	return hex.EncodeToString(d[0][:])
}

// rawLine is one physical line of the input file.
type rawLine struct {
	data    []byte // the line, without its terminator
	off     int64  // byte offset of data[0] within the file
	num     int    // 1-based physical line number
	tooLong bool   // the line exceeded the ceiling and data is truncated
}

// lineScanner reads lines of arbitrary length while tracking byte offsets, which
// bufio.Scanner cannot do and which the scheduler needs in order to read one row
// out of the input blob without holding the whole file.
type lineScanner struct {
	br  *bufio.Reader
	off int64
	num int
	max int
	buf []byte
}

func (s *lineScanner) next() (*rawLine, error) {
	start := s.off
	s.buf = s.buf[:0]
	over := false
	// The ceiling is on the line's content; the terminator does not count
	// against it, or a line of exactly max bytes would be rejected.
	limit := 0
	if s.max > 0 {
		limit = s.max + 1
	}
	for {
		chunk, err := s.br.ReadSlice('\n')
		s.off += int64(len(chunk))
		if limit > 0 && len(s.buf)+len(chunk) > limit {
			over = true
			if room := limit - len(s.buf); room > 0 {
				s.buf = append(s.buf, chunk[:room]...)
			}
		} else {
			s.buf = append(s.buf, chunk...)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == io.EOF {
			if len(s.buf) == 0 {
				return nil, io.EOF
			}
			break
		}
		if err != nil {
			return nil, err
		}
		break
	}
	s.num++
	line := s.buf
	if n := len(line); n > 0 && line[n-1] == '\n' {
		line = line[:n-1]
	}
	if n := len(line); n > 0 && line[n-1] == '\r' {
		line = line[:n-1]
	}
	return &rawLine{data: line, off: start, num: s.num, tooLong: over}, nil
}
