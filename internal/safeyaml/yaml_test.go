package safeyaml

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/parseguard"
)

func TestYAMLResourceLimits(t *testing.T) {
	bomb := "a: &a [x,x,x,x,x,x,x,x,x,x]\n"
	for i := 1; i < 7; i++ {
		prev := "a"
		if i > 1 {
			prev = fmt.Sprintf("n%d", i-1)
		}
		bomb += fmt.Sprintf("n%d: &n%d [%s]\n", i, i, strings.TrimSuffix(strings.Repeat("*"+prev+",", 10), ","))
	}
	largeAlias := "a: &a " + strings.Repeat("x", 100000) + "\nb: [" + strings.TrimSuffix(strings.Repeat("*a,", 400), ",") + "]"
	ratioBomb := "a: &a " + strings.Repeat("x", 1024) + "\nb: [" + strings.TrimSuffix(strings.Repeat("*a,", 1000), ",") + "]"
	for _, b := range []string{
		strings.Repeat("[", 1000) + "0" + strings.Repeat("]", 1000),
		"a: &a [*a]", bomb, largeAlias, ratioBomb,
		"[" + strings.Repeat("0,", maxSeparators) + "0]",
		strings.Repeat("x", int(parseguard.MaxFileBytes)+1),
	} {
		// Checking into Node matters: library alias protections in typed decode
		// do not protect the node tree used by configuration editors.
		var n Node
		if err := Unmarshal([]byte(b), &n); err == nil {
			t.Fatal("hostile YAML accepted")
		}
		if err := NewDecoder(strings.NewReader(b)).Decode(&n); err == nil {
			t.Fatal("decoder accepted hostile YAML")
		}
	}
}

func TestOrdinaryAliasesAndKnownFields(t *testing.T) {
	var v struct {
		A []string `yaml:"a"`
		B []string `yaml:"b"`
	}
	if err := Unmarshal([]byte("a: &a [one,two]\nb: *a"), &v); err != nil || len(v.B) != 2 {
		t.Fatalf("%v %v", v, err)
	}
	d := NewDecoder(strings.NewReader("unknown: true"))
	d.KnownFields(true)
	if err := d.Decode(&v); err == nil {
		t.Fatal("lost strict field checking")
	}
	if err := Unmarshal([]byte(strings.Repeat("[", 129)+"0"+strings.Repeat("]", 129)), &v); !errors.Is(err, parseguard.ErrDepth) {
		t.Fatal(err)
	}
}

func TestWideMappingRejectedBeforeTypedDecode(t *testing.T) {
	var b strings.Builder
	for i := 0; i <= maxMappingPairs; i++ {
		fmt.Fprintf(&b, "k%d: value\n", i)
	}
	var v map[string]string
	if err := Unmarshal([]byte(b.String()), &v); !errors.Is(err, ErrExpansion) || v != nil {
		t.Fatalf("err=%v decoded entries=%d", err, len(v))
	}
}
