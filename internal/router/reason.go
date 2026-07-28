package router

import "github.com/ziozzang/dorang/internal/prefix"

// Reasons a decision can carry. They are constants and table lookups rather
// than formatted strings because §15.5 prohibits formatted string construction
// on the hot path, and a decision reason is produced on every single request.
const (
	ReasonOnlyCandidate = "only_candidate"
	ReasonSticky        = "sticky"
	ReasonPinned        = "pinned"
	ReasonConfigOrder   = "config_order"
)

// prefixHitReasons is "prefix_hit:depth=N" for every depth the chain can
// produce. internal/prefix bounds the chain at MaxDepth, so this table is
// complete rather than a cache with a fallback path.
var prefixHitReasons = func() [prefix.MaxDepth + 1]string {
	var out [prefix.MaxDepth + 1]string
	for i := range out {
		out[i] = "prefix_hit:depth=" + itoa(i)
	}
	return out
}()

// fallbackReasons is "fallback:<cause>" for every cause.
var fallbackReasons = func() [numCauses]string {
	var out [numCauses]string
	for i := range out {
		out[i] = "fallback:" + Cause(i).String()
	}
	return out
}()

func prefixHitReason(depth int) string {
	if depth < 0 || depth >= len(prefixHitReasons) {
		return "prefix_hit"
	}
	return prefixHitReasons[depth]
}

func fallbackReason(c Cause) string {
	if int(c) >= len(fallbackReasons) {
		return "fallback"
	}
	return fallbackReasons[c]
}

// itoa is strconv.Itoa without the import, used only at package init.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
