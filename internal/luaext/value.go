package luaext

import "strconv"

// kind is a policy value's static type. The compiler knows every expression's
// kind before the program ever runs, which is why the evaluator has no type
// errors to report and no nil to trip over.
type kind uint8

const (
	kindNone kind = iota
	kindString
	kindNumber
	kindBool
	kindList
)

func (k kind) String() string {
	switch k {
	case kindString:
		return "string"
	case kindNumber:
		return "number"
	case kindBool:
		return "bool"
	case kindList:
		return "list"
	}
	return "none"
}

// value is one operand. It is a value type with no pointer to anything the
// caller owns, so nothing a program touches can outlive the invocation except
// the constant pool, which is immutable.
type value struct {
	k    kind
	num  int64
	b    bool
	str  string
	list []string
}

func strValue(s string) value    { return value{k: kindString, str: s} }
func numValue(n int64) value     { return value{k: kindNumber, num: n} }
func boolValue(b bool) value     { return value{k: kindBool, b: b} }
func listValue(l []string) value { return value{k: kindList, list: l} }

// String renders a value for a tag or a deny reason.
func (v value) String() string {
	switch v.k {
	case kindString:
		return v.str
	case kindNumber:
		return strconv.FormatInt(v.num, 10)
	case kindBool:
		if v.b {
			return "true"
		}
		return "false"
	}
	return ""
}

// bytes is what the memory ceiling charges for holding this value live.
//
// The struct itself is charged at a fixed size and its string payload at its
// length. A list is charged for its elements; lists only ever come from the
// constant pool, so the charge is for holding a reference to shared data, which
// is the honest accounting: the program did not allocate it.
const valueOverhead = 64

func (v value) bytes() int64 {
	n := int64(valueOverhead) + int64(len(v.str))
	for _, s := range v.list {
		n += int64(len(s)) + 16
	}
	return n
}
