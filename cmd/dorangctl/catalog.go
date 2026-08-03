package main

import (
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/ziozzang/dorang/pkg/catalog"
)

// runCatalog implements `catalog explain`, `catalog unverified` and
// `catalog verify`.
func (e env) runCatalog(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(e.stderr, "usage: dorangctl catalog explain <kind> <model>")
		fmt.Fprintln(e.stderr, "       dorangctl catalog unverified [--state <state>]")
		fmt.Fprintln(e.stderr, "       dorangctl catalog verify --kind <kind> [--key-env VAR] [--write FILE]")
		return 2
	}
	switch args[0] {
	case "explain":
		return e.catalogExplain(args[1:])
	case "unverified":
		return e.catalogUnverified(args[1:])
	case "verify":
		return e.catalogVerify(args[1:])
	}
	return e.fail("unknown catalog subcommand %q", args[0])
}

// catalogExplain is the provenance view (DESIGN §4.3).
//
// An operator whose context window is wrong needs to know which file to edit,
// and an operator who wrote an overlay needs to know whether it applied. Both
// are guesses without this, which is why pkg/catalog records the origin of every
// resolved field and why this prints all of them rather than the value alone.
func (e env) catalogExplain(args []string) int {
	fs := newFlagSet("catalog explain", e)
	overlays := fs.String("catalog", "", "extra model catalog files or directories, comma separated")
	pos, flags := splitLeadingArgs(args)
	if err := fs.Parse(flags); err != nil {
		return 2
	}
	pos = append(pos, fs.Args()...)
	if len(pos) != 2 {
		fmt.Fprintln(e.stderr, "usage: dorangctl catalog explain <kind> <model>")
		return 2
	}
	kind, model := pos[0], pos[1]

	cat, err := catalog.Load(splitComma(*overlays)...)
	if err != nil {
		return e.fail("%v", err)
	}

	// The kind's own resolution first: an alias that points somewhere
	// unexpected explains a whole column of surprising values below it.
	if target, origin, ok := cat.AliasOrigin(kind); ok {
		fmt.Fprintf(e.stdout, "kind %s is an alias for %s (%s %s)\n",
			kind, target, origin.Origin, origin.Source)
	}
	if origins, ok := cat.ExplainKind(kind); ok {
		fmt.Fprintf(e.stdout, "\nkind %s\n", kind)
		writeOrigins(e, origins)
	} else {
		fmt.Fprintf(e.stdout, "\nkind %s is not declared by any layer; model defaults come from the model entry alone\n", kind)
	}

	info := cat.Model(kind, model)
	fmt.Fprintf(e.stdout, "\nmodel %s on kind %s\n", model, kind)
	fmt.Fprintf(e.stdout, "  resolved through layer(s): %s\n", layerList(info.Layers))
	if info.MatchedPrefix != "" {
		fmt.Fprintf(e.stdout, "  matched prefix rule: %q\n", info.MatchedPrefix)
	}
	if !info.ModelKnown {
		fmt.Fprintf(e.stdout, "  (no exact model entry; the values below are kind and prefix defaults)\n")
	}
	writeOrigins(e, cat.Explain(kind, model))

	if r := cat.Reasoning(kind, model); !r.Known() {
		fmt.Fprintf(e.stdout, "\nreasoning capability is UNKNOWN for this model: "+
			"nothing has declared it against a live endpoint\n")
	}
	return 0
}

// writeOrigins prints one field per line: value, which layer won it, and which
// file wrote it.
func writeOrigins(e env, origins []catalog.FieldOrigin) {
	tw := tabwriter.NewWriter(e.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  FIELD\tVALUE\tLAYER\tORIGIN\tSOURCE")
	for _, o := range origins {
		value := o.Value
		if value == "" {
			value = `""`
		}
		origin := string(o.Origin)
		if !o.Declared() {
			origin = "undeclared"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", o.Field, value, dash(o.Layer), origin, dash(o.Source))
	}
	_ = tw.Flush()
}

// stateBlurb is the one-line meaning of each verification state, in the terms
// an operator has to act in. The states differ by what the operator should DO —
// buy entitlement, stop routing to a name, get a credential, send a request —
// and a bare enum name does not carry that.
var stateBlurb = map[catalog.Verification]string{
	catalog.VerificationVerified:     "asked, and it answered as itself",
	catalog.VerificationDenied:       "asked; this plan is not entitled. The model EXISTS",
	catalog.VerificationSubstituted:  "asked; a DIFFERENT model answered",
	catalog.VerificationCitationOnly: "nobody could ask: no credential for the route here",
	catalog.VerificationUnchecked:    "nobody has asked, and nothing says why",
}

// catalogUnverified reports what the catalog knows about its own knowledge.
//
// It used to print one flat list of everything undated, which was accurate and
// useless: an entry nobody had typed sat beside one that had been asked and had
// given a definite answer, and the operator could not tell them apart or tell
// which of them was their problem to fix. The states differ by what to do about
// them, so they are reported apart, findings first — a substitution is a live
// routing defect, while a citation-only row is a credential nobody has.
//
// Reasoning capability is a separate axis with a separate probe (DESIGN §10.2),
// and is summarised at the end rather than mixed in: a model can be verified to
// exist and still have an unknown reasoning control, and folding the two into
// one "unverified" count is what made the old output unreadable.
func (e env) catalogUnverified(args []string) int {
	fs := newFlagSet("catalog unverified", e)
	overlays := fs.String("catalog", "", "extra model catalog files or directories, comma separated")
	state := fs.String("state", "", "list only this state: "+strings.Join(stateNames(), ", "))
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cat, err := catalog.Load(splitComma(*overlays)...)
	if err != nil {
		return e.fail("%v", err)
	}
	by := cat.ModelsByVerification()

	if *state != "" {
		want := catalog.Verification(*state)
		if _, ok := stateBlurb[want]; !ok {
			return e.fail("unknown state %q; want one of %s", *state, strings.Join(stateNames(), ", "))
		}
		for _, ref := range by[want] {
			e.writeStateLine(cat, want, ref)
		}
		return 0
	}

	total := len(cat.Models())
	fmt.Fprintf(e.stdout, "%d catalogued models. What happened the last time an endpoint was asked:\n\n", total)
	tw := tabwriter.NewWriter(e.stdout, 0, 4, 2, ' ', 0)
	for _, v := range catalog.VerificationStates() {
		fmt.Fprintf(tw, "  %s\t%d\t%s\n", v, len(by[v]), stateBlurb[v])
	}
	_ = tw.Flush()

	// The two states that carry a finding, listed with the finding. These are
	// the actionable ones and there are few of them, which is the whole payoff.
	for _, v := range []catalog.Verification{catalog.VerificationDenied, catalog.VerificationSubstituted} {
		if len(by[v]) == 0 {
			continue
		}
		fmt.Fprintf(e.stdout, "\n%s (%d) — %s:\n", v, len(by[v]), stateBlurb[v])
		for _, ref := range by[v] {
			e.writeStateLine(cat, v, ref)
		}
	}

	// Never asked, by kind. Per entry this is 195 lines that all say the same
	// thing; per kind it is the credential list an operator can act on.
	for _, v := range []catalog.Verification{catalog.VerificationUnchecked, catalog.VerificationCitationOnly} {
		if len(by[v]) == 0 {
			continue
		}
		perKind := map[string]int{}
		var order []string
		for _, ref := range by[v] {
			if perKind[ref.Kind] == 0 {
				order = append(order, ref.Kind)
			}
			perKind[ref.Kind]++
		}
		slices.Sort(order)
		fmt.Fprintf(e.stdout, "\n%s (%d across %d kinds) — %s:\n", v, len(by[v]), len(order), stateBlurb[v])
		tw := tabwriter.NewWriter(e.stdout, 0, 4, 2, ' ', 0)
		for _, k := range order {
			fmt.Fprintf(tw, "    %s\t%d\n", k, perKind[k])
		}
		_ = tw.Flush()
		fmt.Fprintf(e.stdout, "  list them with --state %s; close one with "+
			"`dorangctl catalog verify --kind <kind>` and a credential\n", v)
	}

	// The other axis, kept visibly separate.
	unknownReasoning := len(cat.UnverifiedModels())
	fmt.Fprintf(e.stdout, "\nreasoning capability is unknown for %d of %d models. "+
		"That is a different question and a different probe (DESIGN §10.2): "+
		"a model can be verified to exist and still have an unknown reasoning control.\n",
		unknownReasoning, total)
	return 0
}

// writeStateLine prints one entry with whatever evidence its state carries.
func (e env) writeStateLine(cat *catalog.Catalog, v catalog.Verification, ref catalog.ModelRef) {
	// Labelled, never joined: a model name may contain any character, and a
	// joined identifier would invite the splitting REVIEW C3 forbids.
	fmt.Fprintf(e.stdout, "  kind=%s model=%s\n", ref.Kind, ref.Model)
	p := cat.Probe(ref.Kind, ref.Model)
	if p.Served != "" {
		fmt.Fprintf(e.stdout, "      served instead: %s (asked %s)\n", p.Served, p.Date)
	} else if p.Date != "" {
		fmt.Fprintf(e.stdout, "      asked %s\n", p.Date)
	}
	if p.Note != "" {
		fmt.Fprintf(e.stdout, "      %s\n", collapseSpace(p.Note))
	}
}

func stateNames() []string {
	out := make([]string, 0, len(catalog.VerificationStates()))
	for _, v := range catalog.VerificationStates() {
		out = append(out, string(v))
	}
	return out
}

// collapseSpace folds a folded-YAML note onto one line so a list stays scannable.
func collapseSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

func layerList(layers []string) string {
	if len(layers) == 0 {
		return "none"
	}
	return strings.Join(layers, " -> ")
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
