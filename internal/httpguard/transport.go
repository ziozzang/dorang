// Package httpguard disables unused HTTP decompression at client boundaries.
package httpguard

import (
	"net/http"
	"strings"
)

// Transport retains Go's connection defaults but never advertises gzip or
// automatically decompresses a peer-controlled stream.
func Transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DisableCompression = true
	return t
}

func Identity(h http.Header) bool {
	for _, v := range h.Values("Content-Encoding") {
		if v != "" && !strings.EqualFold(strings.TrimSpace(v), "identity") {
			return false
		}
	}
	return true
}
