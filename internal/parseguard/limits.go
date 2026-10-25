// Package parseguard bounds resources before interpreting untrusted documents.
package parseguard

import (
	"errors"
	"io"
	"os"
)

const MaxDepth = 128
const MaxFileBytes int64 = 16 << 20

var ErrDepth = errors.New("document nesting exceeds 128 levels")
var ErrSize = errors.New("document exceeds size limit")

// JSONDepth uses no recursion or allocations and ignores brackets in strings.
// Syntax checking remains the JSON parser's job.
func JSONDepth(b []byte) error {
	return JSONDepthWithin(b, MaxDepth)
}

func JSONDepthWithin(b []byte, maxDepth int) error {
	depth := 0
	quoted, escaped := false, false
	for _, c := range b {
		if quoted {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				quoted = false
			}
			continue
		}
		switch c {
		case '"':
			quoted = true
		case '{', '[':
			depth++
			if depth > maxDepth {
				return ErrDepth
			}
		case '}', ']':
			depth--
		}
	}
	return nil
}

// Read never consumes more than limit+1 bytes and never returns a truncated
// document as a successful read.
func Read(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, ErrSize
	}
	return b, nil
}

func ReadFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("document must be a regular file")
	}
	if info.Size() > MaxFileBytes {
		return nil, ErrSize
	}
	return Read(f, MaxFileBytes)
}
