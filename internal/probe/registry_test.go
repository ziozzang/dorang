package probe

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/quota"
)

func TestNewByEverySpelling(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"zai", "z.ai", "Z-AI", "glm", "zhipu", "bigmodel"} {
		p, err := New(name, Config{})
		if err != nil {
			t.Fatalf("New(%q): %v", name, err)
		}
		// The prober answers to the name it was built with, because
		// quota.Registry dispatches on quota.Credential.ProviderID.
		if p.ProviderID() != name {
			t.Errorf("New(%q).ProviderID() = %q", name, p.ProviderID())
		}
		if p.Endpoint() != zaiBaseURL+zaiPath {
			t.Errorf("New(%q).Endpoint() = %q", name, p.Endpoint())
		}
	}
}

// A provider with no credential-readable endpoint returns the reason rather
// than a prober that reports zeroes. §6.2 takes a reported figure over local
// metering, so a silent zero would read as "nothing used".
func TestProvidersWithoutAnEndpoint(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"openai", "codex", "chatgpt", "xai", "grok",
		"google", "gemini", "vertex", "minimax", "ollama", "qwen", "dashscope",
	} {
		p, err := New(name, Config{})
		if !errors.Is(err, ErrNoEndpoint) {
			t.Errorf("New(%q) = %v, %v; want ErrNoEndpoint", name, p, err)
			continue
		}
		// The reason travels with the error: "there is no endpoint" is only
		// useful with "and here is what was checked".
		if len(err.Error()) < 80 {
			t.Errorf("New(%q) gave no reason: %v", name, err)
		}
	}
}

// Never examined is a different statement from examined and empty.
func TestUnknownProviderIsDistinctFromNoEndpoint(t *testing.T) {
	t.Parallel()
	_, err := New("some-vendor-nobody-checked", Config{})
	if !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("error = %v, want ErrUnknownProvider", err)
	}
	if errors.Is(err, ErrNoEndpoint) {
		t.Error("an unexamined provider was reported as having no endpoint")
	}
}

func TestSupportedTable(t *testing.T) {
	t.Parallel()
	sup := Supported()
	if len(sup) != len(table) {
		t.Fatalf("Supported() = %d rows, want %d", len(sup), len(table))
	}
	// Probed providers first, and every row says something.
	seenUnprobed := false
	for _, s := range sup {
		if !s.Probed() {
			seenUnprobed = true
		} else if seenUnprobed {
			t.Errorf("%s: a probed provider is listed after an unprobed one", s.Provider)
		}
		if s.Note == "" {
			t.Errorf("%s: no note", s.Provider)
		}
		if s.Probed() && s.Auth == "" {
			t.Errorf("%s: a prober with no stated auth", s.Provider)
		}
		if !s.Probed() && s.Endpoint != "" {
			t.Errorf("%s: an endpoint with no prober", s.Provider)
		}
	}
	if got := Probed(); len(got) != 3 || strings.Join(got, ",") != "anthropic,deepseek,zai" {
		t.Errorf("Probed() = %v", got)
	}
}

// Mutating what Supported returns must not edit the table.
func TestSupportedIsACopy(t *testing.T) {
	t.Parallel()
	sup := Supported()
	for i := range sup {
		if len(sup[i].Aliases) > 0 {
			sup[i].Aliases[0] = "clobbered"
			break
		}
	}
	for _, s := range Supported() {
		for _, a := range s.Aliases {
			if a == "clobbered" {
				t.Fatal("Supported() shares the table's slices")
			}
		}
	}
}

func TestAllowanceValidation(t *testing.T) {
	t.Parallel()
	good := Allowance{Label: "tokens_limit:5h", Window: quota.Rolling(5 * time.Hour),
		Metric: quota.MetricTokensTotal, Limit: 1000}
	if err := good.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	for _, tc := range []struct {
		name string
		a    Allowance
	}{
		{"no label", Allowance{Window: good.Window, Metric: good.Metric}},
		{"no window", Allowance{Label: "x", Metric: good.Metric}},
		{"a window shorter than a bucket", Allowance{Label: "x", Window: quota.Rolling(time.Second)}},
		{"a negative limit", Allowance{Label: "x", Window: good.Window, Limit: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.a.Validate(); err == nil {
				t.Error("want an error")
			}
			// And New refuses it, rather than silently ignoring the mapping the
			// operator wrote.
			if _, err := New("zai", Config{Allowances: []Allowance{tc.a}}); err == nil {
				t.Error("New accepted an invalid allowance")
			}
		})
	}
}

func TestConfigDefaults(t *testing.T) {
	t.Parallel()
	p, err := New("zai", Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.cfg.MinInterval != DefaultMinInterval || p.cfg.BackoffBase != DefaultBackoffBase {
		t.Errorf("defaults not applied: %+v", p.cfg)
	}
	if cap(p.sem) != DefaultMaxConcurrent {
		t.Errorf("in-flight cap = %d", cap(p.sem))
	}
	// A ceiling below the base would make the backoff shrink with each failure.
	p2, err := New("zai", Config{BackoffBase: time.Hour, BackoffCeiling: time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p2.cfg.BackoffCeiling < p2.cfg.BackoffBase {
		t.Errorf("ceiling %v below base %v", p2.cfg.BackoffCeiling, p2.cfg.BackoffBase)
	}
}
