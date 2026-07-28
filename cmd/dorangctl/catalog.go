package main

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/ziozzang/dorang/pkg/catalog"
)

// runCatalog implements `catalog explain` and `catalog unverified`.
func (e env) runCatalog(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(e.stderr, "usage: dorangctl catalog explain <kind> <model> | dorangctl catalog unverified")
		return 2
	}
	switch args[0] {
	case "explain":
		return e.catalogExplain(args[1:])
	case "unverified":
		return e.catalogUnverified(args[1:])
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

// catalogUnverified lists the models whose capabilities nobody has confirmed.
//
// It is the probe list: a model here is one dorang will answer questions about
// from data that carries no verification date, and §4.3 is explicit that an
// unverified capability is not the same as an absent one.
func (e env) catalogUnverified(args []string) int {
	fs := newFlagSet("catalog unverified", e)
	overlays := fs.String("catalog", "", "extra model catalog files or directories, comma separated")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cat, err := catalog.Load(splitComma(*overlays)...)
	if err != nil {
		return e.fail("%v", err)
	}
	models := cat.UnverifiedModels()
	if len(models) == 0 {
		fmt.Fprintln(e.stdout, "every catalogued model carries a verification date")
		return 0
	}
	fmt.Fprintf(e.stdout, "%d model(s) need a probe:\n", len(models))
	for _, m := range models {
		fmt.Fprintf(e.stdout, "  %s\n", m)
	}
	return 0
}

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
