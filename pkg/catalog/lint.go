package catalog

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// now is time.Now, indirected so the "a verification date cannot be in the
// future" check is testable without waiting.
var now = time.Now

// Validate reports every problem in a loaded catalog, in one pass.
//
// Loading already refuses data that breaks the schema. Validate is for the
// rest: data that parses, loads, and is still wrong or shaped like a mistake
// that has bitten before. It never stops at the first finding, because an
// operator fixing a catalog wants the whole list, not one item per attempt.
//
// Errors are contradictions — a max output larger than the context window that
// must contain it, a verification date that has not happened yet. Warnings are
// shapes: a kind-wide context window that will be applied to models nobody has
// looked at, a prefix rule that crosses providers.
//
// A warning-only result is a catalog that loads and runs.
func (c *Catalog) Validate() []Problem {
	var ps []Problem
	today := now().UTC().Format(time.DateOnly)

	for _, name := range c.kindOrder {
		kd := c.kinds[name]
		org := c.kindOrigins[name]

		if kd.ContextWindow > 0 && kd.MaxOutputTokens > kd.ContextWindow {
			ps = append(ps, Problem{
				Severity: SeverityError,
				Source:   org[FieldMaxOutputTokens].source,
				Kind:     name,
				Field:    FieldMaxOutputTokens,
				Message: fmt.Sprintf(
					"max_output_tokens %d exceeds context_window %d; the output has to fit in the window that holds it",
					kd.MaxOutputTokens, kd.ContextWindow),
			})
		}
		if kd.Verified != "" && kd.Verified > today {
			ps = append(ps, Problem{
				Severity: SeverityError,
				Source:   org[FieldVerified].source,
				Kind:     name,
				Field:    FieldVerified,
				Message: fmt.Sprintf("verified date %s is in the future; a date records what was checked, and it has not been",
					kd.Verified),
			})
		}
		if d := kd.Probe.Date; d != "" && d > today {
			ps = append(ps, Problem{
				Severity: SeverityError,
				Source:   org[FieldProbe].source,
				Kind:     name,
				Field:    "probe.date",
				Message: fmt.Sprintf("probe date %s is in the future; a date records what an endpoint said, and it has not said it yet",
					d),
			})
		}
		if kd.ContextWindow > 0 {
			ps = append(ps, Problem{
				Severity: SeverityWarning,
				Source:   org[FieldContextWindow].source,
				Kind:     name,
				Field:    FieldContextWindow,
				Message: fmt.Sprintf(
					"kind declares context_window %d, which applies to every model on this kind including ones nobody has looked at; "+
						"prefer declaring the window on the models or prefix rules where it was observed (DESIGN §4.3)",
					kd.ContextWindow),
			})
		}
		if !isChatLike(kd.Category) && (kd.SupportsTools || kd.SupportsStreaming) {
			ps = append(ps, Problem{
				Severity: SeverityWarning,
				Source:   org[FieldCategory].source,
				Kind:     name,
				Field:    FieldCategory,
				Message: fmt.Sprintf("category %q with supports_tools=%t supports_streaming=%t; neither is meaningful on this surface",
					kd.Category, kd.SupportsTools, kd.SupportsStreaming),
			})
		}
	}

	for _, r := range c.rules {
		if r.contextWindow > 0 && r.maxOutputTokens > r.contextWindow {
			ps = append(ps, Problem{
				Severity: SeverityError,
				Source:   r.origins[FieldMaxOutputTokens].source,
				Kind:     r.kind,
				Field:    FieldMaxOutputTokens,
				Message: fmt.Sprintf("prefix rule %q: max_output_tokens %d exceeds context_window %d",
					r.prefix, r.maxOutputTokens, r.contextWindow),
			})
		}
		if r.kind == "" && (r.contextWindow > 0 || r.maxOutputTokens > 0) {
			ps = append(ps, Problem{
				Severity: SeverityWarning,
				Source:   r.origins[FieldContextWindow].source,
				Field:    FieldContextWindow,
				Message: fmt.Sprintf(
					"prefix rule %q applies to every kind and declares a token limit; the same model name is not the same limit on "+
						"a different host (DESIGN §4.3) — scope the rule with `kind:`",
					r.prefix),
			})
		}
	}

	for _, ref := range c.modelRefs {
		e := c.models[ref]
		org := e.origins

		// The composed view, not the entry alone: a kind's max output next to
		// a model's smaller window is the contradiction that actually reaches
		// a request.
		info := c.Model(ref.Kind, ref.Model)
		if info.ContextWindow > 0 && info.MaxOutputTokens > info.ContextWindow {
			ps = append(ps, Problem{
				Severity: SeverityError,
				Source:   org[FieldMaxOutputTokens].source,
				Kind:     ref.Kind,
				Model:    ref.Model,
				Field:    FieldMaxOutputTokens,
				Message: fmt.Sprintf(
					"resolved max_output_tokens %d exceeds resolved context_window %d",
					info.MaxOutputTokens, info.ContextWindow),
			})
		}

		// A slice, not a map: findings must come out in the same order every
		// run or a lint gate becomes flaky for reasons unrelated to the data.
		for _, d := range []struct{ field, date, source string }{
			{FieldVerified, e.verified, org[FieldVerified].source},
			{"reasoning.verified", e.reasoning.Verified, org[FieldVerified].source},
			{"pricing.verified", e.pricing.Verified, org[FieldVerified].source},
			{"probe.date", e.probe.Date, org[FieldProbe].source},
		} {
			field, date := d.field, d.date
			if date != "" && date > today {
				ps = append(ps, Problem{
					Severity: SeverityError,
					Source:   d.source,
					Kind:     ref.Kind,
					Model:    ref.Model,
					Field:    field,
					Message:  fmt.Sprintf("verified date %s is in the future; a date records what was checked, and it has not been", date),
				})
			}
		}

		// A kind that says nobody could ask, in the same file as an entry that
		// says somebody did. Scoped to one source on purpose: an operator with
		// a credential overlaying real dates onto a citation-only kind is the
		// intended workflow, not a defect, and warning about it would make the
		// marker something operators route around.
		if kd, ok := c.kinds[ref.Kind]; ok && kd.Probe.Result == ProbeCitationOnly &&
			(e.verified != "" || !e.probe.IsZero()) &&
			c.kindOrigins[ref.Kind][FieldProbe].source == probeEvidenceSource(e, org) {
			ps = append(ps, Problem{
				Severity: SeverityWarning,
				Source:   c.kindOrigins[ref.Kind][FieldProbe].source,
				Kind:     ref.Kind,
				Model:    ref.Model,
				Field:    FieldProbe,
				Message: fmt.Sprintf("kind %s is marked %q, but this entry in the same file records a live answer; "+
					"one of the two is stale", ref.Kind, ProbeCitationOnly),
			})
		}

		if !e.pricing.IsZero() {
			if e.pricing.Currency == "" {
				ps = append(ps, Problem{
					Severity: SeverityError,
					Source:   org[FieldPricing].source,
					Kind:     ref.Kind,
					Model:    ref.Model,
					Field:    "pricing.currency",
					Message:  "pricing hint has amounts but no currency; a bare number is not a price",
				})
			}
			if e.pricing.Verified == "" {
				ps = append(ps, Problem{
					Severity: SeverityWarning,
					Source:   org[FieldPricing].source,
					Kind:     ref.Kind,
					Model:    ref.Model,
					Field:    "pricing.verified",
					Message:  "pricing hint carries no verified date; it will flow into cost-aware routing regardless (DESIGN §8)",
				})
			}
		}

		if !isChatLike(info.Category) && info.Reasoning.Known() {
			ps = append(ps, Problem{
				Severity: SeverityWarning,
				Source:   org[FieldReasoning].source,
				Kind:     ref.Kind,
				Model:    ref.Model,
				Field:    FieldReasoning,
				Message: fmt.Sprintf("category %q with a concrete reasoning capability %q",
					info.Category, info.Reasoning.Effective()),
			})
		}
	}

	slices.SortStableFunc(ps, func(x, y Problem) int {
		if x.Severity != y.Severity {
			if x.Severity == SeverityError {
				return -1
			}
			return 1
		}
		return 0
	})
	return ps
}

// probeEvidenceSource names the file that supplied whichever live answer this
// entry carries, so the citation-only contradiction can be scoped to one file.
func probeEvidenceSource(e modelEntry, org map[string]fieldSource) string {
	if e.verified != "" {
		return org[FieldVerified].source
	}
	return org[FieldProbe].source
}

func isChatLike(c Category) bool {
	switch c {
	case CategoryChat, CategoryCompletion, "":
		return true
	}
	return false
}

// LintFiles checks operator catalog files without starting anything.
//
// It is the shape a `dorangctl lint` wants: give it the files and directories
// a deployment would load, get back everything wrong with them. Each path may
// be a file or a directory, expanded exactly as [Loader] expands it. The
// embedded data is included as context, so an overlay may refer to kinds it
// does not itself declare.
//
// A malformed file is a finding, never a panic and never a partial result: a
// lint that takes the process down with it has failed at its one job. Findings
// are returned even when some files could not be read at all.
//
// The environment layer is deliberately not consulted — lint what you name.
// To lint what a server would actually load:
//
//	catalog.LintFiles(append(configured, catalog.EnvPaths()...)...)
func LintFiles(paths ...string) (problems []Problem) {
	// A YAML document is attacker-shaped input in some deployments, and this
	// entry point exists to be run against files nobody trusts yet. If the
	// parser ever panics, that is a finding about the file, not a reason to
	// abort the caller.
	defer func() {
		if r := recover(); r != nil {
			problems = append(problems, Problem{
				Severity: SeverityError,
				Message:  fmt.Sprintf("catalog data caused a panic while loading: %v", r),
			})
		}
	}()

	srcs := embeddedSources()
	for _, p := range paths {
		got, err := expand(p, OriginFile)
		if err != nil {
			problems = append(problems, Problem{
				Severity: SeverityError,
				Source:   p,
				Message:  err.Error(),
			})
			continue
		}
		srcs = append(srcs, got...)
	}

	b := newBuilder()
	for _, src := range srcs {
		docs, err := decode(src)
		if err != nil {
			problems = append(problems, Problem{
				Severity: SeverityError,
				Source:   src.name,
				Message:  trimSourcePrefix(err.Error(), src.name),
			})
			continue
		}
		for _, doc := range docs {
			b.add(src, doc)
		}
	}

	c, err := b.build()
	if err != nil {
		for _, e := range flatten(err) {
			problems = append(problems, Problem{
				Severity: SeverityError,
				Message:  e.Error(),
			})
		}
		return problems
	}
	return append(problems, c.Validate()...)
}

// flatten unwraps the joined error build returns into its parts, so each
// defect becomes its own finding instead of one wall of text.
func flatten(err error) []error {
	var out []error
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		if j, ok := e.(interface{ Unwrap() []error }); ok {
			for _, sub := range j.Unwrap() {
				walk(sub)
			}
			return
		}
		out = append(out, e)
	}
	walk(err)
	if len(out) == 0 {
		out = append(out, err)
	}
	return out
}

// trimSourcePrefix drops the leading "<file>: " decode already added, since
// Problem carries the source in its own field.
func trimSourcePrefix(msg, name string) string {
	return strings.TrimPrefix(msg, name+": ")
}
