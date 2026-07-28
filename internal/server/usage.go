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
			u, shape, ok := parseUsageObject(b, i)
			if !ok {
				return u, false
			}
			return normalizeUsage(u, shape), true
		}
		i = skipValue(b, i)
		if i < 0 {
			return Usage{}, false
		}
	}
}

// usageShape records which family's spelling the scanned object used, which is
// the only thing that says what the cache counts mean.
//
// The two families disagree, and they disagree silently because the numbers are
// all plausible either way: OpenAI's prompt_tokens INCLUDES the cached prefix
// and prompt_tokens_details.cached_tokens is the part of it that was cached,
// while Anthropic's input_tokens EXCLUDES cache_read_input_tokens and
// cache_creation_input_tokens, which are counted beside it. Adding the cache
// counts to the wrong one either double-counts the prefix or loses it.
type usageShape uint8

const (
	shapeUnknown usageShape = iota
	// shapeInclusive is the OpenAI family: prompt_tokens already contains the
	// cached prefix. This is also dorang's own convention (canonical.Usage).
	shapeInclusive
	// shapeExclusive is the Anthropic family: input_tokens is cache-exclusive.
	shapeExclusive
)

// normalizeUsage converts a scanned usage object into dorang's own convention —
// Input inclusive of both cache counts, Total = Input + Output — so that a
// relayed response is accounted by the same rule as a converted one.
//
// Without this the ledger holds two definitions again: a passthrough prefix
// pointed at an Anthropic-native surface would record an input count short by
// the whole cached prefix, and a total the client never saw.
func normalizeUsage(u Usage, shape usageShape) Usage {
	if shape == shapeExclusive {
		u.Input += u.CacheRead + u.CacheWrite
	}
	if u.Input == 0 && u.Output == 0 && u.Total > 0 {
		// A body that stated a total and neither half has stated its prompt
		// count: an embedding and a rerank have no completion half, so the
		// total IS the prompt count. internal/backend's scanRelayUsage and the
		// audio decoder already read the field that way, and without the same
		// reading here a passthrough embedding metered as zero tokens — which
		// is also a request §11.6's token guard cannot see.
		u.Input = u.Total
	}
	// ONE definition of "total tokens" in the binary: input plus output, with
	// every breakdown field a subset of one of them. The same function as
	// canonical.Usage.TotalTokens and meter.Tokens.Total, deliberately.
	//
	// A stated total is not preferred over the derived one any more. It was,
	// and that was the second of the three answers to this question: a stated
	// total travelled as far as the tokens-per-minute ceiling (app.totalTokens)
	// and no further, because meter.Tokens carries the five breakdown fields
	// and re-derives, so the ledger and the ceiling could count the same
	// passthrough request differently. A total that contradicts its own parts
	// is not a quantity this gateway can attribute, and the parts are what
	// every other reader is built on.
	u.Total = u.Input + u.Output
	return u
}

// parseUsageObject reads the fields of a usage object starting at b[i] == '{'.
// It returns the counts as the body stated them, unnormalized, plus which
// family's spelling was used; [normalizeUsage] is what converts them.
func parseUsageObject(b []byte, i int) (Usage, usageShape, bool) {
	var u Usage
	shape := shapeUnknown
	found := false
	i++
	for {
		i = skipSpace(b, i)
		if i >= len(b) {
			return u, shape, false
		}
		if b[i] == '}' {
			return u, shape, found
		}
		if b[i] == ',' {
			i++
			continue
		}
		if b[i] != '"' {
			return u, shape, false
		}
		keyStart := i + 1
		keyEnd, esc, ok := scanString(b, i)
		if !ok {
			return u, shape, false
		}
		key := string(b[keyStart : keyEnd-1])
		i = skipSpace(b, keyEnd)
		if i >= len(b) || b[i] != ':' {
			return u, shape, false
		}
		i = skipSpace(b, i+1)
		valStart := i
		i = skipValue(b, i)
		if i < 0 {
			return u, shape, false
		}
		if esc {
			continue
		}
		n, isNum := parseInt(b[valStart:i])
		switch key {
		case "prompt_tokens":
			if isNum {
				u.Input, found = n, true
				shape = shapeInclusive
			}
		case "input_tokens":
			if isNum {
				u.Input, found = n, true
				shape = shapeExclusive
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
			// The nested object never carries an input count, so its shape is
			// not consulted — the enclosing object's spelling decides.
			if sub, _, ok := parseUsageObject(b, valStart); ok {
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
