package app

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/keyguard"
	"github.com/ziozzang/dorang/internal/meter"
	"github.com/ziozzang/dorang/internal/quota"
	"github.com/ziozzang/dorang/internal/server"
)

// The one request every reader below is asked about.
//
// Every breakdown field is non-zero and none of them is equal to the count it is a subset
// of, which is what makes the readers distinguishable: 40 of the 120 prompt tokens came
// from cache and 10 more were written to it, and 5 of the 15 completion tokens were
// reasoning. Any rule that adds a subset back produces a different number.
const (
	ttInput      = 120
	ttOutput     = 15
	ttCacheRead  = 40
	ttCacheWrite = 10
	ttReasoning  = 5

	// ttTotal is the answer. It is input plus output, because the breakdown fields are
	// parts of those two and not addends beside them.
	ttTotal = ttInput + ttOutput // 135

	// The wrong answers, spelled out so a failure names the defect rather than a delta.
	ttPlusReasoning = ttTotal + ttReasoning                              // 140 — the token guard's
	ttSumOfAll      = ttTotal + ttCacheRead + ttCacheWrite + ttReasoning // 190 — the ledger's, once
)

// TestTokenTotalHasOneRule asks every reader of "how many tokens was that" the same
// question and requires the same answer.
//
// This has now been the same defect three times. The wire said 120 and the ledger said 128;
// C1 fixed three sites; a fourth — the §11.6 token guard, seventy lines above one of them —
// still added Reasoning and was fed 128 for the request whose own answer said 120. Fixing
// instances is what produced that sequence, so this test pins the definition instead: a new
// reader either goes through one of the functions below or fails here.
func TestTokenTotalHasOneRule(t *testing.T) {
	canonicalUsage := canonical.Usage{
		InputTokens: ttInput, OutputTokens: ttOutput,
		CacheReadTokens: ttCacheRead, CacheWriteTokens: ttCacheWrite,
		ReasoningTokens: ttReasoning,
	}
	serverUsage := server.Usage{
		Input: ttInput, Output: ttOutput,
		CacheRead: ttCacheRead, CacheWrite: ttCacheWrite, Reasoning: ttReasoning,
		Total: int64(canonicalUsage.TotalTokens()),
	}
	meterTokens := meter.Tokens{
		Input: ttInput, Output: ttOutput,
		CacheRead: ttCacheRead, CacheWrite: ttCacheWrite, Reasoning: ttReasoning,
	}

	for _, r := range []struct {
		name string
		got  int64
	}{
		{"canonical.Usage.TotalTokens — the number on the wire", int64(canonicalUsage.TotalTokens())},
		{"meter.Tokens.Total — the number in the ledger row and the rollup", meterTokens.Total()},
		{"app.totalTokens — the tokens-per-minute ceiling and the metric", totalTokens(serverUsage)},
		{"server.Usage.Total — as the dispatcher fills it", serverUsage.Total},
		{"quota.Usage.Value(MetricTokensTotal) — the per-credential ceiling",
			quota.Usage{TokensInput: ttInput, TokensOutput: ttOutput}.Value(quota.MetricTokensTotal)},
		{"keyguard, through the meter adapter — §11.6's token guard", observedByTheGuard(t, serverUsage)},
	} {
		if r.got == ttTotal {
			continue
		}
		switch r.got {
		case ttPlusReasoning:
			t.Errorf("%s answered %d, want %d: it adds Reasoning, which is already inside "+
				"Output (§10.7)", r.name, r.got, ttTotal)
		case ttSumOfAll:
			t.Errorf("%s answered %d, want %d: it sums all five breakdown fields, which "+
				"counts the cached prefix and the reasoning tokens twice", r.name, r.got, ttTotal)
		default:
			t.Errorf("%s answered %d, want %d", r.name, r.got, ttTotal)
		}
	}
}

// observedByTheGuard runs the real metering adapter and returns the token count it handed
// §11.6's guard.
//
// It goes through meterAdapter.Record rather than through totalTokens directly, because the
// defect was not in the shared function — it was a caller that had written the sum out for
// itself, and only the caller's own path can show that.
func observedByTheGuard(t *testing.T, u server.Usage) int64 {
	t.Helper()
	h := &capturingHistory{}
	g, err := keyguard.New(keyguard.Config{Enabled: true}, h, noopEnforcer{}, nil)
	if err != nil {
		t.Fatalf("keyguard.New: %v", err)
	}
	a := &meterAdapter{
		m:     meter.New(meter.Config{}),
		now:   func() time.Time { return time.Unix(1_800_000_000, 0).UTC() },
		guard: g,
	}
	a.Record(server.Event{KeyID: "k1", Result: server.Result{Tokens: u}})
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.seen) != 1 {
		t.Fatalf("the guard was fed %d observations, want 1", len(h.seen))
	}
	return h.seen[0]
}

type capturingHistory struct {
	mu   sync.Mutex
	seen []int64
}

func (c *capturingHistory) Add(_ context.Context, _ string, n int64, _ time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, n)
	return nil
}

func (c *capturingHistory) Window(context.Context, string, time.Time, time.Time) (int64, error) {
	return 0, nil
}
func (c *capturingHistory) Since(context.Context, string) (time.Time, error) { return time.Time{}, nil }
func (c *capturingHistory) Keys(context.Context) ([]string, error)           { return nil, nil }

type noopEnforcer struct{}

func (noopEnforcer) Pend(context.Context, string, string) error { return nil }
func (noopEnforcer) Revoke(context.Context, string) error       { return nil }
func (noopEnforcer) Release(context.Context, string) error      { return nil }

// ---------------------------------------------------------------------------
// The source-level half
// ---------------------------------------------------------------------------

// TestNoSecondTokenTotalRuleIsWrittenAnywhere fails any expression that adds a token count
// back into the count it is a subset of.
//
// The value test above only reaches the readers it knows about, and the whole lesson of this
// defect is that the sweep misses one. This one does not depend on knowing where they are:
// it reads every non-test source file in the module and refuses the SHAPE — a `+` chain that
// mixes an input/output count with a cache or reasoning count.
//
// The one legitimate place that shape appears is normalization, where an exclusive wire
// count is converted INTO the inclusive form (`InputTokens = InputTokens + CacheRead +
// CacheWrite`, COMPATIBILITY §6.7). Those are recognized structurally rather than listed:
// the result of the sum is itself the input count, so the tokens are moved rather than
// duplicated. Anything else has invented a second rule.
//
// Test files are not scanned. A test may deliberately spell the wrong sum out in order to
// name it — cmd/dorang's `equivDoubleCounted` and this file's `ttSumOfAll` both do.
func TestNoSecondTokenTotalRuleIsWrittenAnywhere(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var findings []string

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", "ui", "bin":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil // a file this build does not compile is not this test's business
		}
		exempt := map[ast.Expr]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			switch s := n.(type) {
			case *ast.AssignStmt:
				for i, lhs := range s.Lhs {
					if i < len(s.Rhs) && isParentCount(leafName(lhs)) {
						markSumChain(s.Rhs[i], exempt)
					}
				}
			case *ast.KeyValueExpr:
				if isParentCount(leafName(s.Key)) {
					markSumChain(s.Value, exempt)
				}
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			b, ok := n.(*ast.BinaryExpr)
			if !ok || b.Op != token.ADD || exempt[b] {
				return true
			}
			var parents, subsets []string
			for _, op := range sumOperands(b) {
				name := leafName(op)
				switch {
				case isSubsetCount(name):
					subsets = append(subsets, name)
				case isParentCount(name):
					parents = append(parents, name)
				}
			}
			if len(parents) > 0 && len(subsets) > 0 {
				findings = append(findings, fset.Position(b.Pos()).String()+
					": adds "+strings.Join(subsets, "+")+" to "+strings.Join(parents, "+"))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Errorf("%s\n\tCacheRead and CacheWrite are parts of the input count and Reasoning is "+
			"part of the output count (§10.7). A sum that adds one back is a second definition "+
			"of \"total tokens\"; use canonical.Usage.TotalTokens, meter.Tokens.Total or "+
			"app.totalTokens.", f)
	}
}

// markSumChain marks e and every `+` node inside it as an intended normalization.
func markSumChain(e ast.Expr, m map[ast.Expr]bool) {
	b, ok := e.(*ast.BinaryExpr)
	if !ok || b.Op != token.ADD {
		return
	}
	m[b] = true
	markSumChain(b.X, m)
	markSumChain(b.Y, m)
}

// sumOperands flattens a `+` chain into its leaves.
func sumOperands(e ast.Expr) []ast.Expr {
	b, ok := e.(*ast.BinaryExpr)
	if !ok || b.Op != token.ADD {
		return []ast.Expr{e}
	}
	return append(sumOperands(b.X), sumOperands(b.Y)...)
}

// leafName is the identifier an expression ends in: the selected field for a selector, the
// name for an identifier, and "" for anything else.
func leafName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.SelectorExpr:
		return v.Sel.Name
	case *ast.Ident:
		return v.Name
	case *ast.StarExpr:
		return leafName(v.X)
	case *ast.ParenExpr:
		return leafName(v.X)
	case *ast.CallExpr:
		return leafName(v.Fun)
	case *ast.IndexExpr:
		return leafName(v.X)
	}
	return ""
}

func isParentCount(name string) bool {
	return hasAnyFold(name, "input", "output", "prompt", "completion")
}

func isSubsetCount(name string) bool {
	return hasAnyFold(name, "cacheread", "cachewrite", "cached", "reasoning")
}

func hasAnyFold(name string, subs ...string) bool {
	l := strings.ToLower(name)
	for _, s := range subs {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}
