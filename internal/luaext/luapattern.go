package luaext

import (
	"context"
	"errors"

	lua "github.com/yuin/gopher-lua"

	"github.com/ziozzang/dorang/internal/luaext/luapat"
)

// Pricing the pattern-matching builtins.
//
// # The hole this closes
//
// [preflight]'s neighbours exist because a builtin can *return* more than it was
// given. The search family is the opposite case and the more dangerous one: it
// returns almost nothing — string.find returns two integers — while the work it
// does to get there is superlinear in the subject. Charging for the result
// charged two values for a call that could run for seconds. Measured, against
// the pattern `.-.-.-@`:
//
//	16 chars    124 µs
//	256 chars   4.05 s
//
// The pattern is the operator's; the *length is the caller's*, so any tenant
// whose text reaches a masking or policy hook could buy seconds of CPU per
// request. None of the three ceilings could see it: the match is one Go call, so
// the instruction counter never ticks, gopher-lua's between-instruction context
// check never runs, and the watchdog can only abandon the goroutine — which then
// keeps a core busy after the request it belonged to is gone.
//
// # How it is closed
//
// The match is *priced before it is run*, by running it against a copy of the
// matcher that counts its own steps ([luapat]). The budget handed to the pricing
// run is everything the invocation has left, so a match may spend the
// invocation's remaining instruction budget and not one step more; whatever it
// actually spent is charged, and a match that would have spent more is refused
// with [ErrInstructionLimit] before gopher-lua's own copy is asked to do it.
//
// This is not an estimate and not a worst case. It is the cost, counted. A
// closed-form bound was tried first and does not work: any bound sound enough to
// stop `.-.-.-@` at 256 bytes also refuses `^%s*(.-)%s*$` — trim, the most
// common idiom in the language — on anything longer than about fifty characters,
// because the pattern's *shape* cannot distinguish a blowup from a scan. Only
// running it can.
//
// # What it costs
//
// The match runs twice: once to price it, once to do it. That is the deliberate
// trade — a doubled constant on an operation that is now bounded, in exchange
// for gopher-lua's own string library continuing to produce every result, so
// there is no second dialect of Lua patterns to keep in step. The doubled cost
// is itself bounded, because the priced run is bounded and the real run does the
// same work.
//
// The gas exchange rate makes this honest rather than arbitrary: a charged Lua
// instruction and a matcher step both cost about 22 ns on the machine this was
// written on, so a pattern step and a loop iteration buy the same amount of the
// same budget.

// matchDataBytes is charged per match a search collects.
//
// gsub and gmatch collect *every* match before returning one, so a match set is
// host memory proportional to the subject. luapat charges gas for each match it
// keeps, which bounds how many there can be; this charges the memory ceiling for
// them too, so the two ceilings agree about a match set instead of only one of
// them seeing it.
const matchDataBytes = 64

// price runs the match against the step-counted matcher and charges what it
// cost, aborting the invocation if the budget could not cover it.
//
// It returns the matches it found, because the search is only half of what the
// caller is about to do: gsub replaces at every one of them, and the replacement
// is priced from this set rather than guessed at. A nil return means the price
// is not known — the operator turned the instruction ceiling off — and a caller
// that would have charged per match must then charge nothing, because there is
// no budget for it to come out of.
//
// A malformed pattern is *not* reported here. gopher-lua raises its own error
// for that a moment later, with the message a plugin author has seen before;
// duplicating it here would mean two spellings of the same mistake.
func price(v *vmState, subject, pattern string, offset, limit int) []*luapat.MatchData {
	if v.noGas {
		// The operator turned the instruction ceiling off. Pricing a call
		// against a budget that does not exist would only pay the double cost
		// and buy nothing.
		return nil
	}
	run := luapat.Run{Budget: v.gasLeft, Done: v.done}
	mds, err := luapat.Find(pattern, []byte(subject), offset, limit, &run)
	// run.Budget is what the matcher did not spend; it goes negative by exactly
	// the step that ran out, so this is the true cost either way and v.gas is
	// what decides whether it was affordable.
	v.gas(v.gasLeft - run.Budget)
	switch {
	case errors.Is(err, luapat.ErrBudget):
		// Unreachable while the ceiling is on — the charge above already
		// exceeded the budget it came from — but a ceiling that depends on
		// arithmetic staying in one order is a claim, not a guarantee.
		v.abort(ErrInstructionLimit)
	case errors.Is(err, luapat.ErrDepth):
		// The stack ceiling, and the reason it must abort rather than fall
		// through: gopher-lua's own matcher would recurse exactly as deep, so
		// letting the real call proceed is letting the stack grow that far.
		v.abort(ErrPatternTooDeep)
	case errors.Is(err, context.Canceled):
		// Nobody is waiting for this answer. Ending here is what keeps a priced
		// match from outliving the request that asked for it — the whole reason
		// the matcher is handed the invocation's context.
		v.abort(context.Canceled)
	}
	v.charge(int64(len(mds)) * matchDataBytes)
	return mds
}

// preFind prices string.find(s, pattern [, init [, plain]]).
func preFind(v *vmState, L *lua.LState, n int) {
	s, pat, ok := subjectAndPattern(L)
	if !ok || len(pat) == 0 {
		return
	}
	// The fourth argument turns the call into a plain substring search, which
	// does not go near the matcher and is linear in the subject. gopher-lua
	// tests GetTop() == 4 exactly, so an explicit nil in that slot is still a
	// pattern match; this has to agree with it or the price is for a different
	// call than the one that runs.
	if n == 4 && lua.LVAsBool(L.Get(4)) {
		v.gas(int64(len(s)) + 1)
		return
	}
	price(v, s, pat, stringIndex(s, optIndex(L, 3, 1), true), 1)
}

// preMatch prices string.match(s, pattern [, init]).
func preMatch(v *vmState, L *lua.LState, n int) {
	s, pat, ok := subjectAndPattern(L)
	if !ok {
		return
	}
	// string.match resolves its init differently from string.find — it does the
	// arithmetic inline rather than through luaIndex2StringIndex — so this
	// mirrors that one rather than sharing the other.
	offset := optIndex(L, 3, 1)
	if offset < 0 {
		offset = len(s) + offset + 1
	}
	offset--
	if offset < 0 {
		offset = 0
	}
	price(v, s, pat, offset, 1)
}

// preGmatch prices string.gmatch(s, pattern).
//
// The iterator gmatch returns is a plain host function that is never charged,
// which would be a hole if it did any searching — it does not. gopher-lua finds
// every match eagerly, inside this call, and the iterator only walks the slice.
// So the whole cost is here, and charging it here charges all of it.
func preGmatch(v *vmState, L *lua.LState, n int) {
	s, pat, ok := subjectAndPattern(L)
	if !ok {
		return
	}
	price(v, s, pat, 0, -1)
}

// subjectAndPattern reads the two arguments every search shares.
//
// A call whose arguments are not strings is not priced: gopher-lua's CheckString
// raises before any matching happens, so there is nothing to pay for.
func subjectAndPattern(L *lua.LState) (subject, pattern string, ok bool) {
	sv, pv := L.Get(1), L.Get(2)
	if !isStringy(sv) || !isStringy(pv) {
		return "", "", false
	}
	return lua.LVAsString(sv), lua.LVAsString(pv), true
}

func isStringy(lv lua.LValue) bool {
	switch lv.Type() {
	case lua.LTString, lua.LTNumber:
		return true
	}
	return false
}

// optIndex reads an optional integer argument the way LState.OptInt does. A
// value it cannot read is left at the default, because gopher-lua will raise on
// it before the match runs.
func optIndex(L *lua.LState, idx, def int) int {
	lv := L.Get(idx)
	if !isStringy(lv) {
		return def
	}
	return int(lua.LVAsNumber(lv))
}

// stringIndex is gopher-lua's luaIndex2StringIndex, which turns a Lua 1-based,
// possibly negative index into an offset into the subject. It is duplicated
// rather than approximated: an offset larger than the real one would price a
// shorter search than the one that runs.
func stringIndex(s string, i int, start bool) int {
	if start && i != 0 {
		i--
	}
	l := len(s)
	if i < 0 {
		i = l + i + 1
	}
	if i < 0 {
		i = 0
	}
	if !start && i > l {
		i = l
	}
	return i
}
