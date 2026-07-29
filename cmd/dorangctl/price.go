package main

import (
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ziozzang/dorang/internal/app"
	"github.com/ziozzang/dorang/internal/pricing"
)

// runPrice is the §8.4 preview: the applied rule chain per class, each
// component's rate and quantity, the subtotal, the final amount, and why each
// rule was selected.
//
// It runs internal/pricing's own Explain, so the number here, the number the
// admin calculator shows and the number the ledger records are the same number
// produced by the same code.
func (e env) runPrice(args []string) int {
	fs := newFlagSet("price", e)
	cfgPath := configPathFlag(fs)
	var (
		input      = fs.Int64("input", 0, "input tokens")
		output     = fs.Int64("output", 0, "output tokens")
		cachedRead = fs.Int64("cached-read", 0, "cached input tokens")
		cacheWrite = fs.Int64("cache-write", 0, "cache write tokens")
		reasoning  = fs.Int64("reasoning", 0, "reasoning tokens")
		characters = fs.Int64("characters", 0, "characters, for per-1k-character rules")
		// Two second axes, because a second of wall time and a second of recorded
		// audio are different billable quantities (DESIGN §10.7). Naming them apart
		// here is the same decision the catalog makes: there is no --seconds, so a
		// figure cannot be handed to the wrong rate by leaving the axis unsaid.
		computeSeconds = fs.Float64("compute-seconds", 0,
			"wall seconds the request took, for per_compute_second rules")
		audioSeconds = fs.Float64("audio-seconds", 0,
			"seconds of recorded audio the vendor billed, for per_audio_second rules")
		requests   = fs.Int64("requests", 1, "request count, for per-request rules")
		credential = fs.String("credential", "", "price as this credential instead of the deployment's first")
		at         = fs.String("at", "", "price at this RFC 3339 instant instead of now")
	)
	pos, flags := splitLeadingArgs(args)
	if err := fs.Parse(flags); err != nil {
		return 2
	}
	pos = append(pos, fs.Args()...)
	if len(pos) != 1 {
		fmt.Fprintln(e.stderr, "usage: dorangctl price <model> --input N --output N")
		return 2
	}
	model := pos[0]

	cfg, ok := e.loadConfigFile(*cfgPath)
	if !ok {
		return 1
	}
	target, ok := app.ResolveTarget(cfg, model)
	if !ok {
		return e.fail("no model group or alias named %q", model)
	}
	cat, err := app.Pricing(cfg)
	if err != nil {
		return e.fail("%v", err)
	}

	when := time.Now()
	if *at != "" {
		when, err = time.Parse(time.RFC3339, *at)
		if err != nil {
			return e.fail("--at: %v", err)
		}
	}
	cred := target.Credential
	if *credential != "" {
		cred = *credential
	}

	req := pricing.Request{
		Provider:         target.Provider,
		Model:            target.UpstreamModel,
		Credential:       cred,
		Deployment:       target.Deployment,
		InputTokens:      *input,
		OutputTokens:     *output,
		CacheReadTokens:  *cachedRead,
		CacheWriteTokens: *cacheWrite,
		ReasoningTokens:  *reasoning,
		Characters:       *characters,
		Seconds:          *computeSeconds,
		AudioSeconds:     *audioSeconds,
		Requests:         *requests,
		At:               when,
	}
	x := cat.Explain(req)
	if x.Err != "" {
		return e.fail("%s", x.Err)
	}

	cur := x.Currency
	if cur == "" {
		cur = "USD"
	}
	fmt.Fprintf(e.stdout, "model %s -> group %s, provider %s, upstream model %s\n",
		model, target.Group, target.Provider, target.UpstreamModel)
	if cred != "" {
		fmt.Fprintf(e.stdout, "credential %s\n", cred)
	}
	fmt.Fprintf(e.stdout, "priced at %s in %s\n\n", when.Format(time.RFC3339), cur)

	writeComponents(e, x.Cost.Components, cur)
	writeClasses(e, x.Classes)

	fmt.Fprintf(e.stdout, "\nmarginal      %s %s\n", cur, formatNano(x.Cost.MarginalNano))
	fmt.Fprintf(e.stdout, "subscription  %s %s   (amortized; routing never sees this)\n",
		cur, formatNano(x.Cost.SubscriptionNano))
	fmt.Fprintf(e.stdout, "adjustments   %s %s\n", cur, formatNano(x.Cost.AdjustmentNano))
	fmt.Fprintf(e.stdout, "TOTAL         %s %s\n", cur, formatNano(x.Cost.TotalNano))

	// §8.5: the notional figure is what this traffic would have cost at
	// pay-as-you-go list rates. It is never billed and never routed on, so it is
	// printed apart from the total rather than under it — and with its
	// provenance, because an estimate that cannot be traced to a source and a
	// date is a guess wearing a currency symbol.
	fmt.Fprintln(e.stdout)
	switch {
	case x.Notional.Missing:
		fmt.Fprintf(e.stdout, "notional      unavailable — no notional_rate rule matched "+
			"(not zero: a zero here would make a subscription look infinitely efficient)\n")
	default:
		fmt.Fprintf(e.stdout, "notional      %s %s   (never billed, never routed on)\n",
			cur, formatNano(x.Notional.Nano))
		fmt.Fprintf(e.stdout, "  rule %s", dash(x.Notional.RuleID))
		if x.Notional.Source != "" {
			fmt.Fprintf(e.stdout, ", source %s", x.Notional.Source)
		}
		if x.Notional.AsOfText != "" {
			fmt.Fprintf(e.stdout, ", as of %s (%s old)",
				x.Notional.AsOfText, x.Notional.Age.Round(24*time.Hour))
		}
		fmt.Fprintln(e.stdout)
		if leverage := x.Notional.Nano - x.Cost.TotalNano; x.Cost.TotalNano > 0 && leverage != 0 {
			fmt.Fprintf(e.stdout, "  difference against the bill: %s %s\n", cur, formatNano(leverage))
		}
	}

	if x.Cost.Missing {
		fmt.Fprintf(e.stderr, "\nwarning: no marginal_usage rule matched; this request would be "+
			"recorded UNPRICED rather than as costing zero (§8.3)\n")
	}
	if x.Cost.NoPrice != pricing.NoPriceNone {
		fmt.Fprintf(e.stderr, "\nwarning: rule %s matched and could not price this request on "+
			"%s — %s. It would be recorded UNPRICED rather than as costing zero (§8.3, §10.7)\n",
			x.Cost.NoPriceRule, x.Cost.NoPriceQuantity, x.Cost.NoPrice.Why())
	}
	for _, note := range x.Notes {
		fmt.Fprintf(e.stderr, "note: %s\n", note)
	}
	return 0
}

func writeComponents(e env, cs []pricing.Component, cur string) {
	if len(cs) == 0 {
		fmt.Fprintln(e.stdout, "no priced components")
		return
	}
	tw := tabwriter.NewWriter(e.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "COMPONENT\tRULE\tRATE\tUNIT\tQUANTITY\tSUBTOTAL (%s)\n", cur)
	for _, c := range cs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			c.Name, c.RuleID, c.Rate, c.Unit, formatQuantity(c.Quantity, c.Scale),
			formatNano(c.SubtotalNano))
	}
	_ = tw.Flush()
}

// writeClasses prints why each rule won or lost. Classes compose rather than
// compete (§8.1), so each one reports its own selection separately.
func writeClasses(e env, classes []pricing.ClassTrace) {
	for _, ct := range classes {
		if len(ct.Considered) == 0 {
			continue
		}
		fmt.Fprintf(e.stdout, "\n%s\n", ct.Class)
		tw := tabwriter.NewWriter(e.stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "  RULE\tLEVEL\tPRIORITY\tELIGIBLE\tSELECTED\tWHY")
		for _, c := range ct.Considered {
			fmt.Fprintf(tw, "  %s\t%s\t%d\t%t\t%t\t%s\n",
				c.RuleID, c.Level, c.Priority, c.Eligible, c.Selected, c.Reason)
		}
		_ = tw.Flush()
	}
}

// formatNano renders a nano-unit amount as an exact decimal. Money is never
// printed through a float here: the value is exact in the ledger and it stays
// exact on the way to a terminal.
func formatNano(n int64) string {
	sign := ""
	if n < 0 {
		sign, n = "-", -n
	}
	whole := n / 1_000_000_000
	frac := n % 1_000_000_000
	s := fmt.Sprintf("%s%d.%09d", sign, whole, frac)
	// Trim trailing zeros but keep at least two decimals, which is what a
	// currency amount reads as.
	s = strings.TrimRight(s, "0")
	if i := strings.IndexByte(s, '.'); i >= 0 && len(s)-i < 3 {
		s += strings.Repeat("0", 3-(len(s)-i))
	}
	return s
}

// formatQuantity renders a component quantity, honouring the negative power of
// ten it is expressed in (seconds arrive as micro-seconds).
func formatQuantity(q int64, scale int32) string {
	if scale <= 0 {
		return strconv.FormatInt(q, 10)
	}
	div := int64(1)
	for i := int32(0); i < scale; i++ {
		div *= 10
	}
	return fmt.Sprintf("%d.%0*d", q/div, scale, q%div)
}
