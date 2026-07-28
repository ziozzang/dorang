package server

import "strconv"

// scanUsage extracts a usage object from a JSON response body.
//
// This is the "if the response is JSON with a usage object, price it" half of
// DESIGN §10.6 step 5, and it is the only place the passthrough engine looks
// inside a body at all. It reads the top-level "usage" object and nothing else:
// it does not validate the response, does not care what protocol produced it,
// and returns false rather than guessing when the shape is unfamiliar.
//
// Both naming conventions are accepted because both arrive on the same
// deployment — prompt/completion from the OpenAI family, input/output from the
// Anthropic family — and a passthrough prefix is by definition pointed at a
// native provider surface, so the engine does not get to choose which.
func scanUsage(b []byte) (Usage, bool) {
	i := skipSpace(b, 0)
	if i >= len(b) || b[i] != '{' {
		return Usage{}, false
	}
	i++
	for {
		i = skipSpace(b, i)
		if i >= len(b) || b[i] == '}' {
			return Usage{}, false
		}
		if b[i] == ',' {
			i++
			continue
		}
		if b[i] != '"' {
			return Usage{}, false
		}
		keyStart := i + 1
		keyEnd, esc, ok := scanString(b, i)
		if !ok {
			return Usage{}, false
		}
		key := b[keyStart : keyEnd-1]
		i = skipSpace(b, keyEnd)
		if i >= len(b) || b[i] != ':' {
			return Usage{}, false
		}
		i = skipSpace(b, i+1)
		if !esc && string(key) == "usage" && i < len(b) && b[i] == '{' {
			return parseUsageObject(b, i)
		}
		i = skipValue(b, i)
		if i < 0 {
			return Usage{}, false
		}
	}
}

// parseUsageObject reads the fields of a usage object starting at b[i] == '{'.
func parseUsageObject(b []byte, i int) (Usage, bool) {
	var u Usage
	found := false
	i++
	for {
		i = skipSpace(b, i)
		if i >= len(b) {
			return u, false
		}
		if b[i] == '}' {
			return u, found
		}
		if b[i] == ',' {
			i++
			continue
		}
		if b[i] != '"' {
			return u, false
		}
		keyStart := i + 1
		keyEnd, esc, ok := scanString(b, i)
		if !ok {
			return u, false
		}
		key := string(b[keyStart : keyEnd-1])
		i = skipSpace(b, keyEnd)
		if i >= len(b) || b[i] != ':' {
			return u, false
		}
		i = skipSpace(b, i+1)
		valStart := i
		i = skipValue(b, i)
		if i < 0 {
			return u, false
		}
		if esc {
			continue
		}
		n, isNum := parseInt(b[valStart:i])
		switch key {
		case "prompt_tokens", "input_tokens":
			if isNum {
				u.Input, found = n, true
			}
		case "completion_tokens", "output_tokens":
			if isNum {
				u.Output, found = n, true
			}
		case "total_tokens":
			if isNum {
				u.Total, found = n, true
			}
		case "cache_read_input_tokens", "cached_tokens":
			if isNum {
				u.CacheRead, found = n, true
			}
		case "cache_creation_input_tokens":
			if isNum {
				u.CacheWrite, found = n, true
			}
		case "reasoning_tokens":
			if isNum {
				u.Reasoning, found = n, true
			}
		case "prompt_tokens_details", "completion_tokens_details":
			// Nested detail objects carry cached and reasoning counts on the
			// OpenAI surface. One level down is worth reading; deeper is not.
			if sub, ok := parseUsageObject(b, valStart); ok {
				if sub.CacheRead > 0 {
					u.CacheRead, found = sub.CacheRead, true
				}
				if sub.Reasoning > 0 {
					u.Reasoning, found = sub.Reasoning, true
				}
			}
		}
	}
}

// parseInt reads a JSON integer, rejecting anything else.
func parseInt(b []byte) (int64, bool) {
	if len(b) == 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
