package luaext

import (
	"errors"
	"fmt"
	"time"
)

// Hook names one extension point of DESIGN §11.5.
type Hook uint8

const (
	// HookRequest runs after the request is decoded and before it is routed.
	// It is the only hook whose deny is honoured.
	HookRequest Hook = iota
	// HookRoute runs after the router has chosen a deployment and before the
	// upstream call. It sees the decision — provider, deployment, upstream
	// model — and may refuse it. It cannot ask for a different one; see the
	// package documentation for why.
	HookRoute
	// HookResponse runs after the answer is complete. It observes and
	// annotates; it cannot change the answer, which has already been written.
	HookResponse
	// HookEmail runs before a notification is delivered (DESIGN §11.5's `lua`
	// email driver). It may suppress the message.
	HookEmail
	// HookFilterRequest is DESIGN §10.5b's transform filter: it sees the
	// request's text segments and may rewrite them, which is what a masking
	// plugin needs and what none of the other four hooks can do.
	//
	// It is not part of extensions.lua.hooks. A filter is attached to a model
	// (`models[].filters`), so the operator has already said where it runs, and
	// requiring a second, easily forgotten declaration would mean a configured
	// masking filter that silently does not.
	//
	// There is deliberately no on_filter_response. The response side of a
	// reversible mask is unmasking, and unmasking runs once per streamed frame
	// inside §10.5's single pass. Calling into an untrusted VM there would put a
	// sandbox on the token path — per frame, per request — for a hook whose only
	// legitimate job the host already does exactly.
	HookFilterRequest

	numHooks = 5
)

// hookNames is indexed by Hook. The first four match config's luaHooks exactly;
// a mismatch would let a configured hook name resolve to nothing.
var hookNames = [numHooks]string{"on_request", "on_route", "on_response", "on_email", "on_filter_request"}

// String returns the configuration spelling of the hook.
func (h Hook) String() string {
	if int(h) >= len(hookNames) {
		return "unknown"
	}
	return hookNames[h]
}

// ParseHook resolves a configuration spelling.
func ParseHook(s string) (Hook, bool) {
	for i, n := range hookNames {
		if n == s {
			return Hook(i), true
		}
	}
	return 0, false
}

// Limits are the three ceilings of DESIGN §11.5.
//
// All three are per *invocation*, not per program: a hook point with three
// programs registered gets one budget between them, so adding a program cannot
// multiply the ceiling the operator configured. A program that exhausts the
// shared budget aborts the whole invocation, which fails open.
type Limits struct {
	// Instructions bounds how many virtual instructions one invocation may
	// execute. A policy program cannot loop, so this is a bound on program
	// size times rule count rather than on divergence — see the package
	// documentation. Zero disables the check.
	Instructions int64
	// MemoryBytes bounds the peak live bytes a program may hold: its operand
	// stack plus the tags it has set. It is not enforceable against a [Native]
	// hook, which is ordinary Go code.
	MemoryBytes int64
	// Timeout is the wall-clock ceiling, enforced by the caller against a
	// watchdog goroutine. Zero means no watchdog, which is only appropriate in
	// tests: without it a [Native] hook can block a request indefinitely.
	Timeout time.Duration
}

// setDefaults fills the limits a caller left at zero.
//
// These match internal/config's defaults for extensions.lua.limits, restated
// here so the package is usable without the configuration layer and so a test
// that builds an Engine directly gets a bounded one rather than an unbounded
// one.
func (l *Limits) setDefaults() {
	if l.Instructions == 0 {
		l.Instructions = 5_000_000
	}
	if l.MemoryBytes == 0 {
		l.MemoryBytes = 32 << 20
	}
	if l.Timeout == 0 {
		l.Timeout = 200 * time.Millisecond
	}
}

// Ceiling errors. Each of these is a skip-and-warn (fail-open); none of them
// can refuse a request.
var (
	// ErrInstructionLimit reports an exhausted instruction budget.
	ErrInstructionLimit = errors.New("luaext: instruction ceiling exceeded")
	// ErrMemoryLimit reports an exhausted memory budget.
	ErrMemoryLimit = errors.New("luaext: memory ceiling exceeded")
	// ErrTimeout reports that the wall-clock ceiling elapsed with the hook
	// still running. The hook is abandoned, not waited for.
	ErrTimeout = errors.New("luaext: wall-clock ceiling exceeded")
	// ErrTripped reports that a hook has been switched off because too many of
	// its invocations were abandoned.
	ErrTripped = errors.New("luaext: hook tripped off after repeated abandonment")
	// ErrLuaSource reports a .lua file under the *policy* directory. Lua is
	// executable here, but only when an operator names the file in the
	// configuration: a plugin picked up from a writable directory is the
	// code-execution primitive §11.5 refuses.
	ErrLuaSource = errors.New("luaext: a Lua plugin is loaded by explicit configuration, not by a directory scan")
)

// PanicError wraps a value recovered from a hook. A panicking hook is contained
// and counted; it never reaches the request.
type PanicError struct {
	Hook  Hook
	Unit  string
	Value any
	Stack []byte
}

func (e *PanicError) Error() string {
	return fmt.Sprintf("luaext: %s: %s panicked: %v", e.Hook, e.Unit, e.Value)
}

// Tag is one annotation a hook set with `set`. Tags are copied out by value and
// are the only channel by which a hook adds information to a request.
type Tag struct {
	Name  string
	Value string
}

// maxTags is how many tags one invocation may set. It is fixed so that a
// decision is a value type and carrying it costs no allocation.
const maxTags = 4

// tagset is the fixed-size tag accumulator shared by every decision type.
type tagset struct {
	tags [maxTags]Tag
	n    int
}

// Tags returns the tags set by this invocation.
func (t *tagset) Tags() []Tag { return t.tags[:t.n] }

// Tag returns the value of a named tag.
func (t *tagset) Tag(name string) (string, bool) {
	for i := 0; i < t.n; i++ {
		if t.tags[i].Name == name {
			return t.tags[i].Value, true
		}
	}
	return "", false
}

// set records a tag. Past [maxTags] it overwrites the last slot rather than
// growing, because a decision that allocates would put an allocation on the
// path of every hooked request in exchange for information nobody asked for.
func (t *tagset) set(name, value string) {
	for i := 0; i < t.n; i++ {
		if t.tags[i].Name == name {
			t.tags[i].Value = value
			return
		}
	}
	if t.n < maxTags {
		t.tags[t.n] = Tag{Name: name, Value: value}
		t.n++
		return
	}
	t.tags[maxTags-1] = Tag{Name: name, Value: value}
}
