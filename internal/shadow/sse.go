package shadow

import (
	"bytes"
	"encoding/json"
)

// sseFrame is one parsed event.
type sseFrame struct {
	// event is the `event:` line's value, "" when there was none. Chat
	// completions never send one and messages always do (COMPATIBILITY §1.1,
	// §6.1), which is itself a structural property worth comparing.
	event string
	// data is the concatenation of the frame's `data:` lines, newline-joined,
	// as the SSE specification defines it.
	data string
	// fields is the set of field names the frame carried, so that a stream
	// which starts emitting `id:` lines is noticed.
	fields []string
	// complete records that a blank line terminated the frame. A frame without
	// one is the tail of a truncated capture, not a protocol violation.
	complete bool
}

// parseSSE splits an event stream into frames.
//
// dropFirst and dropLast exist because a truncated capture is a prefix and a
// suffix of the real body: the prefix ends mid-frame and the suffix begins
// mid-frame. Half a frame classifies as a different kind than the whole one, so
// the partials are dropped rather than parsed — a fabricated frame kind in the
// report is worse than a missing one, because a missing one is accounted for by
// the inconclusive record and a fabricated one is not.
func parseSSE(b []byte, dropFirst, dropLast bool) []sseFrame {
	if len(b) == 0 {
		return nil
	}
	var (
		out     []sseFrame
		cur     sseFrame
		data    []byte
		hasData bool
		started bool
	)
	flush := func(complete bool) {
		if !started {
			return
		}
		if hasData {
			cur.data = string(data)
		}
		cur.complete = complete
		out = append(out, cur)
		cur = sseFrame{}
		data = data[:0]
		hasData = false
		started = false
	}

	for len(b) > 0 {
		var line []byte
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			line, b = b[:i], b[i+1:]
		} else {
			line, b = b, nil
		}
		line = bytes.TrimSuffix(line, []byte("\r"))

		if len(line) == 0 {
			flush(true)
			continue
		}
		started = true
		if line[0] == ':' {
			cur.fields = appendUnique(cur.fields, "comment")
			continue
		}
		name, value := line, []byte(nil)
		if i := bytes.IndexByte(line, ':'); i >= 0 {
			name, value = line[:i], line[i+1:]
			value = bytes.TrimPrefix(value, []byte(" "))
		}
		cur.fields = appendUnique(cur.fields, string(name))
		switch string(name) {
		case "event":
			cur.event = string(value)
		case "data":
			if hasData {
				data = append(data, '\n')
			}
			data = append(data, value...)
			hasData = true
		}
	}
	// A trailing frame with no blank line after it is incomplete.
	flush(false)

	if dropFirst && len(out) > 0 {
		out = out[1:]
	}
	if dropLast && len(out) > 0 && !out[len(out)-1].complete {
		out = out[:len(out)-1]
	}
	return out
}

// tailStartsAtBoundary reports whether b begins at a frame boundary rather than
// part-way through one.
//
// A tail window is a fixed number of bytes off the end of a body, so it usually
// begins in the middle of a line. The alternative to this check is to always
// drop the tail's first frame, which throws away a real frame whenever the
// window happens to land on a boundary — and for a short stream that real frame
// is often the terminator, which is the one thing the tail exists to carry.
func tailStartsAtBoundary(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	if b[0] == '\n' || b[0] == '\r' || b[0] == ':' {
		return true
	}
	line := b
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		line = b[:i]
	}
	i := bytes.IndexByte(line, ':')
	if i < 0 {
		return false
	}
	switch string(line[:i]) {
	case "event", "data", "id", "retry":
		return true
	}
	return false
}

func appendUnique(dst []string, s string) []string {
	for _, v := range dst {
		if v == s {
			return dst
		}
	}
	return append(dst, s)
}

// classifyFrame names a frame's kind and returns its decoded body.
//
// The kind is what the run-length-compressed sequence is built from, so it has
// to be coarse enough that model output does not change it and fine enough that
// a protocol change does. For messages the `event:` name is already exactly
// that. For chat completions there is no event name (COMPATIBILITY §1.1), so
// the kind is derived from which of the mutually exclusive chunk roles the
// frame is playing — role, content, tool call, finish, usage — which is the
// same partition the compatibility contract itself uses.
func classifyFrame(f *sseFrame) (kind string, body any) {
	if f.event != "" {
		kind = "event:" + f.event
	}
	if f.data == "" {
		if kind == "" {
			kind = "empty"
		}
		return kind, nil
	}
	if f.data == "[DONE]" {
		return "done", nil
	}
	var v any
	if err := json.Unmarshal([]byte(f.data), &v); err != nil {
		if kind == "" {
			kind = "raw"
		}
		return kind, nil
	}
	if kind != "" {
		return kind, v
	}

	m, ok := v.(map[string]any)
	if !ok {
		return "data", v
	}
	switch {
	case m["error"] != nil || m["type"] == "error":
		return "error", v
	}
	if choices, ok := m["choices"].([]any); ok && len(choices) > 0 {
		if c, ok := choices[0].(map[string]any); ok {
			if fr, ok := c["finish_reason"].(string); ok && fr != "" {
				return "finish", v
			}
			if d, ok := c["delta"].(map[string]any); ok {
				switch {
				case d["tool_calls"] != nil:
					return "tool_calls", v
				case d["content"] != nil:
					return "content", v
				case d["refusal"] != nil:
					return "refusal", v
				case d["role"] != nil:
					return "role", v
				}
			}
		}
	}
	if m["usage"] != nil {
		return "usage", v
	}
	return "data", v
}
