package shadow

import "math"

// sampler decides which requests are shadowed.
//
// Three properties, and each one is load-bearing for a claim §14.1 makes.
//
//   - **Deterministic in the request id.** A retry of one logical call carries
//     the same id (COMPATIBILITY §7.8), so it is sampled the same way twice
//     instead of being charged twice against a ceiling that is meant to bound
//     one day of comparison, not one day of retries. It also means a fleet
//     samples coherently: every node that touches a request agrees about it.
//
//   - **Blind to everything but the id.** Not the model, not the body size, not
//     the price. A sampler that looked at cost — even indirectly, by skipping
//     large bodies or by admitting only what fits the remaining ceiling —
//     would systematically compare the cheap paths and leave the expensive
//     ones uncompared, while the ceiling still read as under-spent and the
//     coverage still read as complete. That is the single most dangerous thing
//     this package could get wrong, because it makes an empty report mean
//     nothing while looking like it means everything.
//
//   - **Uniform.** FNV's low bits mix well and its high bits do not, and the
//     threshold comparison is on the high bits, so the hash is finalized before
//     it is used. Without it a rate of 0.05 is not one in twenty.
//
// It is independent of the metering sampler (DESIGN §12.2) by construction: the
// salt differs, so a request being traced says nothing about it being shadowed.
// Correlated samplers would concentrate both costs on the same traffic.
type sampler struct {
	rate      float64
	threshold uint64
}

// shadowSalt keeps this sampler's decisions independent of any other
// id-keyed sampler in the process.
const shadowSalt = "dorang.shadow\x00"

func newSampler(rate float64) sampler {
	s := sampler{rate: rate}
	switch {
	case rate <= 0:
		s.threshold = 0
	case rate >= 1:
		s.threshold = math.MaxUint64
	default:
		// math.MaxUint64 is not exactly representable as a float64; scaling by
		// 2^64 in float space and converting back is the accurate form.
		s.threshold = uint64(rate * 18446744073709551616.0)
	}
	return s
}

// admit reports whether id is sampled in.
func (s sampler) admit(id string) bool {
	switch s.threshold {
	case 0:
		return false
	case math.MaxUint64:
		return true
	}
	return mix64(fnv64a(shadowSalt, id)) < s.threshold
}

const (
	fnvOffset64 = 14695981039346656037
	fnvPrime64  = 1099511628211
)

// fnv64a hashes salt+s without allocating. hash/fnv would need a []byte
// conversion, and this runs on the request path.
func fnv64a(salt, s string) uint64 {
	h := uint64(fnvOffset64)
	for i := 0; i < len(salt); i++ {
		h ^= uint64(salt[i])
		h *= fnvPrime64
	}
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= fnvPrime64
	}
	return h
}

// mix64 is splitmix64's finalizer.
func mix64(h uint64) uint64 {
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	h *= 0xc4ceb9fe1a85ec53
	h ^= h >> 33
	return h
}
