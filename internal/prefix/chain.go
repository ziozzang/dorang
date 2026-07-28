// Package prefix routes a request to whichever backend most likely still holds
// its conversation prefix in cache.
//
// The hard part is not finding a match — it is proving the match is honest.
// Hashing a set of messages, or each message independently, collides when the
// same messages arrive in a different order, and a collision here sends the
// request to a backend whose cache holds something else entirely.
//
// A hash chain makes that structurally impossible:
//
//	h₀ = H(group)
//	hᵢ = H(hᵢ₋₁ ‖ cᵢ ‖ len(cᵢ))
//
// A match at depth i proves c₁..cᵢ are byte-identical *and in that order*.
// Reordering, insertion, and deletion all diverge, because each step consumes
// the previous digest. Mixing in the segment length removes the boundary
// ambiguity that would otherwise let two different splits of the same bytes
// hash alike. The length is a suffix rather than a prefix for one practical
// reason: a trailing partial segment's length is not known until it ends, and
// putting it last is what lets the whole chain be computed from a stream
// without ever holding the body.
//
// Segments are cut on BYTE boundaries, never token boundaries. Cutting on
// tokens would mean tokenizing the whole conversation before routing, which
// costs more than the entire latency budget on a long conversation and
// contradicts the rule against buffering full request bodies. The chain
// property never depended on tokens.
//
// Segment lengths grow geometrically (4 KiB, 8, 16, 32 …), so a request of n
// bytes produces O(log n) segments and O(log n) table entries while still
// distinguishing prefixes that share a long common head. A fixed shallow depth
// cannot do that: two conversations sharing a system prompt and diverging
// afterwards collide at the deepest tracked node, and "longest common prefix
// wins" quietly becomes false. Measured: a 16 MiB body yields 13 digests where
// fixed 4 KiB chunking would yield 4096.
package prefix

import (
	"crypto/sha256"
	"encoding/binary"
	"hash"
)

// Digest identifies a prefix. It is the first 16 bytes of the chain state,
// which is ample: a collision costs a cache miss, never a wrong answer, so
// 128 bits is far past the point of mattering.
type Digest [16]byte

// DefaultBaseSegment is the size of the first segment, and the granularity at
// which short conversations are distinguished.
const DefaultBaseSegment = 4 << 10 // 4 KiB

// MaxDepth bounds the chain. With a 4 KiB base and geometric growth, depth 24
// spans far more than any real request, so this is a runaway guard rather than
// a tuning knob.
const MaxDepth = 24

// Chain computes prefix digests incrementally as request bytes arrive.
//
// Bytes stream straight into the segment hash and are never retained, so the
// chain costs one hash pass and no allocation proportional to body size.
//
// The zero value is not usable; call NewChain. A Chain is not safe for
// concurrent use — one belongs to one request.
type Chain struct {
	state   [sha256.Size]byte
	seg     hash.Hash // hash of the segment currently being filled
	segLen  int       // bytes written into seg so far
	base    int
	target  int // size at which the current segment closes
	depth   int
	digests []Digest
	sealed  bool
}

// NewChain starts a chain seeded with the tenant and then the model group:
//
//	h₀ = H(tenant ‖ 0x00 ‖ group)
//
// Seeding with the group means two groups can never share an entry, so a match
// always implies the candidate set is the same one. Seeding with the TENANT
// first means two tenants can never share one either, and that is a
// confidentiality property rather than a routing one.
//
// DESIGN §7.4b specified h₀ = H(group_id) alone, and on a shared gateway that
// is not enough. A prefix table keyed on the group is a byte-exact oracle over
// other tenants' prompt prefixes: a caller sends candidate bytes, reads
// prefix_hit:depth=N off its own routing header, and learns that somebody else
// recently sent exactly those bytes on that model. It is also a poisoning
// primitive, because prefix affinity outranks cost in the default strategy
// chain, so planting an entry steers another tenant's next request onto a
// deployment of the planter's choosing. §7.4a already required the tenant to
// lead its key; the two need the same rule.
//
// The 0x00 separator is what stops "ab" ‖ "c" and "a" ‖ "bc" seeding alike —
// neither component may contain a NUL, and a tenant id is derived from stored
// ids rather than from anything a caller writes.
//
// baseSegment of zero uses DefaultBaseSegment.
func NewChain(tenant, group string, baseSegment int) *Chain {
	if baseSegment <= 0 {
		baseSegment = DefaultBaseSegment
	}
	c := &Chain{
		base:    baseSegment,
		target:  baseSegment,
		seg:     sha256.New(),
		digests: make([]Digest, 0, 8),
	}
	h := sha256.New()
	h.Write([]byte(tenant))
	h.Write([]byte{0})
	h.Write([]byte(group))
	h.Sum(c.state[:0])
	c.seg.Write(c.state[:])
	return c
}

// Write feeds request bytes in order, closing every segment they complete.
// It never retains p.
func (c *Chain) Write(p []byte) (int, error) {
	n := len(p)
	if c.sealed {
		return n, nil
	}
	for len(p) > 0 {
		if c.depth >= MaxDepth {
			return n, nil
		}
		need := c.target - c.segLen
		if len(p) < need {
			c.seg.Write(p)
			c.segLen += len(p)
			return n, nil
		}
		c.seg.Write(p[:need])
		c.segLen += need
		p = p[need:]
		c.closeSegment()
	}
	return n, nil
}

// closeSegment folds the accumulated segment into the chain and records the
// digest at this depth. The next segment is twice as long, which is what keeps
// the entry count logarithmic in request size.
func (c *Chain) closeSegment() {
	var lenbuf [8]byte
	binary.BigEndian.PutUint64(lenbuf[:], uint64(c.segLen))
	c.seg.Write(lenbuf[:])
	c.seg.Sum(c.state[:0])

	var d Digest
	copy(d[:], c.state[:])
	c.digests = append(c.digests, d)

	c.depth++
	c.segLen = 0
	if c.target < 1<<30 {
		c.target *= 2
	}
	c.seg.Reset()
	c.seg.Write(c.state[:])
}

// Seal closes any trailing partial segment and returns every digest, shallowest
// first.
//
// The trailing partial is deliberately included. Without it a request shorter
// than one full segment would produce no digest at all, and short conversations
// are exactly the ones that benefit most from landing on a warm backend. It is
// distinguishable from a full segment because its length is folded in.
func (c *Chain) Seal() []Digest {
	if !c.sealed {
		if c.segLen > 0 && c.depth < MaxDepth {
			c.closeSegment()
		}
		c.sealed = true
	}
	return c.digests
}

// Digests returns the digests sealed so far without sealing. Useful when the
// body is still streaming and a routing decision is wanted from what has
// already arrived.
func (c *Chain) Digests() []Digest { return c.digests }

// Depth reports how many segments have been closed.
func (c *Chain) Depth() int { return c.depth }

// Compute is the whole-buffer convenience form of the streaming API above.
func Compute(tenant, group string, body []byte, baseSegment int) []Digest {
	c := NewChain(tenant, group, baseSegment)
	_, _ = c.Write(body)
	return c.Seal()
}
