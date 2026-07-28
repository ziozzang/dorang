package pricing

import (
	"errors"
	"math/bits"
	"strings"
)

// ErrOverflow is returned when a value cannot be represented in the range this package
// guarantees. It is returned instead of a wrapped, truncated or negative amount: §8.3
// requires overflow to be an error, never a negative cost.
var ErrOverflow = errors.New("pricing: amount out of range")

const (
	// attoPerNano is the internal working scale. Amounts are carried as signed 128-bit
	// atto-units (10^-18 of the catalog currency) and converted to nano exactly once.
	attoPerNano = 1_000_000_000
	// attoScale is the number of decimal places in an atto-unit.
	attoScale = 18
	// maxPriceScale is the largest number of fractional digits a price may declare.
	// It is 12 because a component's exact value is rate x quantity / unitDivisor, the
	// largest unitDivisor is 10^6 (per_1m_tokens), and 12 + 6 = 18 keeps every product
	// exactly representable at the atto scale. A price finer than 10^-12 currency units
	// is rejected loudly rather than silently truncated.
	maxPriceScale = 12
	maxInt64      = 1<<63 - 1
)

var pow10 = [...]uint64{
	1, 10, 100, 1e3, 1e4, 1e5, 1e6, 1e7, 1e8, 1e9,
	1e10, 1e11, 1e12, 1e13, 1e14, 1e15, 1e16, 1e17, 1e18, 1e19,
}

// u128 is an unsigned 128-bit integer. It exists so that rate x quantity is formed at
// full width before any division, per §8.3.
type u128 struct{ hi, lo uint64 }

func u64To128(v uint64) u128 { return u128{lo: v} }

func (a u128) isZero() bool { return a.hi == 0 && a.lo == 0 }

func (a u128) cmp(b u128) int {
	if a.hi != b.hi {
		if a.hi < b.hi {
			return -1
		}
		return 1
	}
	if a.lo != b.lo {
		if a.lo < b.lo {
			return -1
		}
		return 1
	}
	return 0
}

// add reports ok=false on wraparound.
func (a u128) add(b u128) (u128, bool) {
	lo, c := bits.Add64(a.lo, b.lo, 0)
	hi, c2 := bits.Add64(a.hi, b.hi, c)
	return u128{hi: hi, lo: lo}, c2 == 0
}

// sub requires a >= b; it reports ok=false on borrow.
func (a u128) sub(b u128) (u128, bool) {
	lo, br := bits.Sub64(a.lo, b.lo, 0)
	hi, br2 := bits.Sub64(a.hi, b.hi, br)
	return u128{hi: hi, lo: lo}, br2 == 0
}

func (a u128) shr(n uint) u128 {
	switch {
	case n == 0:
		return a
	case n >= 128:
		return u128{}
	case n >= 64:
		return u128{lo: a.hi >> (n - 64)}
	}
	return u128{hi: a.hi >> n, lo: a.lo>>n | a.hi<<(64-n)}
}

func (a u128) bitLen() int {
	if a.hi != 0 {
		return 64 + bits.Len64(a.hi)
	}
	return bits.Len64(a.lo)
}

// mulDiv computes a*b/d at 192-bit intermediate width, returning the quotient, the exact
// remainder, and whether the quotient fits in 128 bits. This is the only multiplication
// primitive in the price path: the product is never narrowed before the division.
func (a u128) mulDiv(b, d uint64) (q u128, r uint64, ok bool) {
	if d == 0 {
		return u128{}, 0, false
	}
	// 192-bit product, limbs p2:p1:p0.
	h0, l0 := bits.Mul64(a.lo, b)
	h1, l1 := bits.Mul64(a.hi, b)
	p0 := l0
	p1, c := bits.Add64(l1, h0, 0)
	p2 := h1 + c // cannot wrap: h1 <= 2^64-2 whenever a.hi and b are both < 2^64.

	var q2, q1, q0, rem uint64
	q2, rem = bits.Div64(0, p2, d) // rem < d holds inductively, so Div64 never panics.
	q1, rem = bits.Div64(rem, p1, d)
	q0, rem = bits.Div64(rem, p0, d)
	return u128{hi: q1, lo: q0}, rem, q2 == 0
}

// mul is mulDiv with d = 1.
func (a u128) mul(b uint64) (u128, bool) {
	q, _, ok := a.mulDiv(b, 1)
	return q, ok
}

// amt is a signed exact amount in atto-units. Sign-magnitude keeps the 128-bit primitives
// unsigned and makes the discount case (a negative adjustment) explicit rather than a
// wraparound waiting to happen.
type amt struct {
	neg bool
	m   u128
}

func attoAmt(m u128) amt { return amt{m: m} }

func (a amt) negate() amt {
	if a.m.isZero() {
		return a
	}
	return amt{neg: !a.neg, m: a.m}
}

func addAmt(a, b amt) (amt, bool) {
	if a.neg == b.neg {
		m, ok := a.m.add(b.m)
		return amt{neg: a.neg && !m.isZero(), m: m}, ok
	}
	if a.m.cmp(b.m) >= 0 {
		m, _ := a.m.sub(b.m)
		return amt{neg: a.neg && !m.isZero(), m: m}, true
	}
	m, _ := b.m.sub(a.m)
	return amt{neg: b.neg && !m.isZero(), m: m}, true
}

// mulDivAmt computes a*b/d keeping the sign of a.
func mulDivAmt(a amt, b, d uint64) (amt, bool) {
	m, _, ok := a.m.mulDiv(b, d)
	return amt{neg: a.neg && !m.isZero(), m: m}, ok
}

// roundToNano converts an exact atto amount to nano-units, rounding half-to-even exactly
// once, and returns the signed sub-nano remainder to carry into the next settlement.
// carry is the remainder left by the previous settlement of the same bucket; its magnitude
// is always below one nano.
func roundToNano(v amt, carry int64) (nano int64, newCarry int64, err error) {
	c := amt{neg: carry < 0}
	if carry < 0 {
		c.m = u64To128(uint64(-carry))
	} else {
		c.m = u64To128(uint64(carry))
	}
	t, ok := addAmt(v, c)
	if !ok {
		return 0, carry, ErrOverflow
	}
	q, r, ok := t.m.mulDiv(1, attoPerNano)
	if !ok {
		return 0, carry, ErrOverflow
	}
	var inc uint64
	if twice := r * 2; twice > attoPerNano || (twice == attoPerNano && q.lo&1 == 1) {
		inc = 1
	}
	if inc == 1 {
		q, ok = q.add(u64To128(1))
		if !ok {
			return 0, carry, ErrOverflow
		}
	}
	if q.hi != 0 || q.lo > maxInt64 {
		return 0, carry, ErrOverflow
	}
	n := int64(q.lo)
	rem := int64(r) - int64(inc)*attoPerNano
	if t.neg {
		n, rem = -n, -rem
	}
	return n, rem, nil
}

// addNano adds two nano values with a range check. Sums are checked before they are
// returned or stored, per §8.3.
func addNano(a, b int64) (int64, error) {
	s := a + b
	if (a > 0 && b > 0 && s < 0) || (a < 0 && b < 0 && s >= 0) {
		return 0, ErrOverflow
	}
	return s, nil
}

// decimal is an exact decimal literal: value = (-1)^neg * units * 10^-scale.
// Prices reach this type straight from their string form in the catalog; no price value
// ever passes through a float64.
type decimal struct {
	neg   bool
	units uint64
	scale uint8
	text  string
}

// atto returns the value scaled to atto-units. scale <= maxPriceScale guarantees the
// shift is exact and the result fits in 128 bits.
func (d decimal) atto() (u128, bool) {
	return u64To128(d.units).mul(pow10[attoScale-int(d.scale)])
}

// scaled returns units * 10^(want-scale), used to express a decimal at a coarser fixed
// scale (for example micro-seconds). It fails if the decimal is finer than want.
func (d decimal) scaled(want uint8) (uint64, bool) {
	if d.scale > want {
		return 0, false
	}
	p := pow10[want-d.scale]
	hi, lo := bits.Mul64(d.units, p)
	if hi != 0 {
		return 0, false
	}
	return lo, true
}

var errBadDecimal = errors.New("pricing: not an exact decimal")

// parseDecimal parses a decimal literal. Exponent notation is rejected rather than
// silently routed through a float parser, and more than maxPriceScale fractional digits
// is an error rather than a silent truncation.
func parseDecimal(s string) (decimal, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return decimal{}, errBadDecimal
	}
	d := decimal{text: t}
	i := 0
	if t[0] == '+' || t[0] == '-' {
		d.neg = t[0] == '-'
		i++
	}
	body := t[i:]
	if body == "" {
		return decimal{}, errBadDecimal
	}
	if strings.IndexByte(body, 'e') >= 0 || strings.IndexByte(body, 'E') >= 0 {
		return decimal{}, errors.New("pricing: exponent notation is not accepted; write the digits out")
	}
	var intPart, fracPart string
	if dot := strings.IndexByte(body, '.'); dot >= 0 {
		intPart, fracPart = body[:dot], body[dot+1:]
		if strings.IndexByte(fracPart, '.') >= 0 {
			return decimal{}, errBadDecimal
		}
	} else {
		intPart = body
	}
	if intPart == "" && fracPart == "" {
		return decimal{}, errBadDecimal
	}
	if !allDigits(intPart) || !allDigits(fracPart) {
		return decimal{}, errBadDecimal
	}
	fracPart = strings.TrimRight(fracPart, "0")
	if len(fracPart) > maxPriceScale {
		return decimal{}, errors.New("pricing: at most 12 fractional digits are supported")
	}
	d.scale = uint8(len(fracPart))
	for _, part := range [2]string{intPart, fracPart} {
		for j := 0; j < len(part); j++ {
			hi, lo := bits.Mul64(d.units, 10)
			if hi != 0 {
				return decimal{}, ErrOverflow
			}
			sum, c := bits.Add64(lo, uint64(part[j]-'0'), 0)
			if c != 0 {
				return decimal{}, ErrOverflow
			}
			d.units = sum
		}
	}
	if d.units == 0 {
		d.neg, d.scale = false, 0
	}
	return d, nil
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
