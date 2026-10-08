package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ziozzang/dorang/internal/app"
	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/store"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// `dorangctl spend reconcile` compares what a vendor's ACCOUNT says it billed with
// what the ledger recorded, per vendor account, for one calendar month (UTC).
//
// It is report-only. It reads the ledger, calls each vendor's usage endpoint with
// the credential the gateway itself uses, and writes nothing anywhere.
//
// The comparison that is expected to agree is the vendor's figure against the
// ledger's REPORTED column — what the upstream stated on each response — and not
// against `cost`: dorang's own figure carries plan shares, adjustments and rate
// cards the vendor never saw, so a gap there is a statement about the rate card,
// not a defect in the extraction.
//
// A vendor account is a SECRET, not a provider: three providers sharing one key
// (a chat provider, a scoring provider and a decisions provider on one OpenRouter
// key) are one account on the vendor's side, so their ledger rows are summed.

func (e env) runSpend(args []string) int {
	if len(args) == 0 || args[0] != "reconcile" {
		fmt.Fprintln(e.stderr, "usage: dorangctl spend reconcile [--config FILE] [--period YYYY-MM] [--provider NAMES]")
		return 2
	}
	return e.spendReconcile(args[1:])
}

func (e env) spendReconcile(args []string) int {
	fs := newFlagSet("spend reconcile", e)
	cfgPath := configPathFlag(fs)
	var (
		period    = fs.String("period", "", "calendar month, YYYY-MM (UTC); default the current one")
		providers = fs.String("provider", "", "comma-separated provider names; default every provider whose kind declares usage_api")
		timeout   = fs.Duration("timeout", 30*time.Second, "per vendor request")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	start, end, err := monthRange(*period, time.Now().UTC())
	if err != nil {
		return e.fail("%v", err)
	}
	cfg, ok := e.loadConfigFile(*cfgPath)
	if !ok {
		return 1
	}
	cat, err := compareCatalog(syncOpts{})
	if err != nil {
		return e.fail("catalog: %v", err)
	}
	accounts, err := reconcileAccounts(cfg, cat, splitComma(*providers))
	if err != nil {
		return e.fail("%v", err)
	}
	if len(accounts) == 0 {
		return e.fail("no configured provider's kind declares usage_api")
	}
	ctx := context.Background()
	st, err := app.OpenStore(ctx, cfg, false)
	if err != nil {
		return e.fail("%v", err)
	}
	defer st.Close()

	client := &http.Client{Timeout: *timeout}
	failed := false
	for _, a := range accounts {
		ledger, err := a.ledger(ctx, st, store.TimeRange{Start: start, End: end})
		if err != nil {
			return e.fail("ledger: %v", err)
		}
		vendor, verr := a.fetch(ctx, client, start, end)
		e.printAccount(a, start, ledger, vendor, verr)
		failed = failed || verr != nil
	}
	if failed {
		return 1
	}
	return 0
}

// account is one vendor account: a usage API, a base URL and a secret, with every
// credential that authenticates as it.
type account struct {
	api         string
	base        string
	secret      string
	label       string // how the secret is named in the configuration
	providers   []string
	credentials []string
}

// reconcileAccounts groups the configured credentials by vendor account.
func reconcileAccounts(cfg *config.Config, cat *catalog.Catalog, only []string) ([]*account, error) {
	want := map[string]bool{}
	for _, p := range only {
		want[p] = true
	}
	type prov struct{ api, base string }
	provs := map[string]prov{}
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if len(want) > 0 && !want[p.Name] {
			continue
		}
		kd, ok := cat.Kind(p.Kind)
		if !ok || kd.UsageAPI == "" {
			continue
		}
		base := p.BaseURL
		if base == "" {
			base = kd.BaseURL
		}
		provs[p.Name] = prov{kd.UsageAPI, base}
	}
	byKey := map[string]*account{}
	var out []*account
	for i := range cfg.Credentials {
		c := &cfg.Credentials[i]
		pv, ok := provs[c.Provider]
		if !ok || c.IsOAuth() {
			continue
		}
		secret, resolved := c.Key.Value()
		if !resolved || secret == "" {
			return nil, fmt.Errorf("credential %q (provider %q) has no resolved secret in this environment",
				c.ID, c.Provider)
		}
		// The host is part of the account: one secret sent to two hosts is two
		// accounts as far as either host's usage endpoint can say.
		host, err := apiHost(pv.base)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", c.Provider, err)
		}
		k := pv.api + "\x00" + host + "\x00" + secret
		a := byKey[k]
		if a == nil {
			a = &account{api: pv.api, base: pv.base, secret: secret, label: secretLabel(c.Key)}
			byKey[k] = a
			out = append(out, a)
		}
		if !contains(a.providers, c.Provider) {
			a.providers = append(a.providers, c.Provider)
		}
		a.credentials = append(a.credentials, c.ID)
	}
	return out, nil
}

func secretLabel(s config.SecretRef) string {
	switch {
	case s.Env != "":
		return "$" + s.Env
	case s.File != "":
		return s.File
	}
	return "inline"
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// ledger sums the account's credentials per upstream model.
func (a *account) ledger(ctx context.Context, st *store.Store, r store.TimeRange) ([]store.ModelSpend, error) {
	by := map[string]*store.ModelSpend{}
	for _, id := range a.credentials {
		rows, err := st.SpendByCredentialModel(ctx, id, r)
		if err != nil {
			return nil, err
		}
		for _, m := range rows {
			cur := by[m.UpstreamModel]
			if cur == nil {
				cur = &store.ModelSpend{UpstreamModel: m.UpstreamModel}
				by[m.UpstreamModel] = cur
			}
			cur.Requests += m.Requests
			cur.CostNano += m.CostNano
			cur.ReportedNano += m.ReportedNano
			cur.ReportedRequests += m.ReportedRequests
		}
	}
	out := make([]store.ModelSpend, 0, len(by))
	for _, m := range by {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpstreamModel < out[j].UpstreamModel })
	return out, nil
}

// vendorUsage is what a vendor's account says it billed.
type vendorUsage struct {
	// scope says what the figure covers, in words, because the two vendors'
	// endpoints disagree about it and the reader must not assume.
	scope string
	// totalUSD is the vendor's total for the period.
	totalUSD float64
	// models is the per-model breakdown when the vendor offers one.
	models []vendorModel
}

// vendorModel is one model's vendor lines, summed. A model is billed on several
// axes (input and output tokens are two items), so units are kept per axis.
type vendorModel struct {
	model string
	units []string
	usd   float64
}

func (a *account) fetch(ctx context.Context, c *http.Client, start, end time.Time) (*vendorUsage, error) {
	switch a.api {
	case catalog.UsageAPIDeepInfra:
		return fetchDeepInfra(ctx, c, a, start, end)
	case catalog.UsageAPIOpenRouter:
		return fetchOpenRouter(ctx, c, a, start, end)
	}
	return nil, fmt.Errorf("usage_api %q has no reader in this build", a.api)
}

// apiHost is the scheme and host of a provider's base URL.
func apiHost(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("base_url %q has no scheme and host", base)
	}
	return u.Scheme + "://" + u.Host, nil
}

func getJSON(ctx context.Context, c *http.Client, url, secret string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %d: %s", req.URL.Path, resp.StatusCode, strings.TrimSpace(string(body[:min(len(body), 300)])))
	}
	return json.Unmarshal(body, into)
}

// fetchDeepInfra reads GET /payment/usage, the account's per-model usage.
//
// Measured 2026-10-09: the endpoint takes `from`/`to` either as YYYY.MM periods
// (bucketed in the account's own time zone, not UTC) or as epoch seconds spanning
// at most 31 days and ending no later than now, so the UTC month is asked for in
// seconds and the open month up to now. Each item states
// `units`, `rate` and `cost` with both money fields in US CENTS, and `cost` is
// rounded to a whole cent — units x rate is the exact figure.
//
// It is ACCOUNT-wide: every key on the account, not only the one dorang uses.
func fetchDeepInfra(ctx context.Context, c *http.Client, a *account, start, end time.Time) (*vendorUsage, error) {
	host, err := apiHost(a.base)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Months []struct {
			Items []struct {
				Model struct {
					Name string `json:"model_name"`
				} `json:"model"`
				Units       float64 `json:"units"`
				RateCents   float64 `json:"rate"`
				PricingType string  `json:"pricing_type"`
			} `json:"items"`
		} `json:"months"`
	}
	// `to` may not lie in the future (refused with the same message as an
	// over-long range), so the open month is asked for up to now.
	if now := time.Now(); end.After(now) {
		end = now
	}
	u := fmt.Sprintf("%s/payment/usage?from=%d&to=%d", host, start.Unix(), end.Unix())
	if err := getJSON(ctx, c, u, a.secret, &doc); err != nil {
		return nil, err
	}
	out := &vendorUsage{scope: "account-wide: every key on the DeepInfra account"}
	at := map[string]int{}
	for _, m := range doc.Months {
		for _, it := range m.Items {
			usd := it.Units * it.RateCents / 100
			out.totalUSD += usd
			i, ok := at[it.Model.Name]
			if !ok {
				i = len(out.models)
				at[it.Model.Name] = i
				out.models = append(out.models, vendorModel{model: it.Model.Name})
			}
			out.models[i].usd += usd
			out.models[i].units = append(out.models[i].units, fmt.Sprintf("%g %s", it.Units, it.PricingType))
		}
	}
	return out, nil
}

// fetchOpenRouter reads GET /api/v1/key, the calling key's own usage.
//
// Measured 2026-10-09: an ordinary key states its usage as running totals —
// `usage_daily`, `usage_weekly`, `usage_monthly` and all-time — in USD, and the
// per-model activity endpoint answers 403 to anything but a management key. So
// only the CURRENT month can be reconciled, and only as a total.
func fetchOpenRouter(ctx context.Context, c *http.Client, a *account, start, end time.Time) (*vendorUsage, error) {
	now := time.Now().UTC()
	if now.Before(start) || !now.Before(end) {
		return nil, fmt.Errorf("OpenRouter states a key's usage for the current month only; %s is not it",
			start.Format("2006-01"))
	}
	host, err := apiHost(a.base)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Data struct {
			UsageMonthly float64 `json:"usage_monthly"`
		} `json:"data"`
	}
	if err := getJSON(ctx, c, host+"/api/v1/key", a.secret, &doc); err != nil {
		return nil, err
	}
	return &vendorUsage{
		scope:    "this key only, current UTC month, total (no per-model breakdown without a management key)",
		totalUSD: doc.Data.UsageMonthly,
	}, nil
}

func (e env) printAccount(a *account, start time.Time, ledger []store.ModelSpend, v *vendorUsage, verr error) {
	w := e.stdout
	fmt.Fprintf(w, "== %s account %s — providers %s — %s\n", a.api, a.label,
		strings.Join(a.providers, ", "), start.Format("2006-01"))

	var reqs, stated int64
	var cost, reported int64
	for _, m := range ledger {
		reqs += m.Requests
		stated += m.ReportedRequests
		cost += m.CostNano
		reported += m.ReportedNano
	}
	if verr != nil {
		fmt.Fprintf(w, "vendor:  unavailable: %v\n", verr)
	} else {
		fmt.Fprintf(w, "vendor:  %s USD (%s)\n", usd(v.totalUSD), v.scope)
	}
	fmt.Fprintf(w, "ledger:  %d requests; reported %s USD on %d of them; dorang cost %s USD\n",
		reqs, nanoUSD(reported), stated, nanoUSD(cost))
	if verr == nil {
		fmt.Fprintf(w, "gap:     vendor - ledger reported = %s USD\n", usd(v.totalUSD-float64(reported)/1e9))
	}

	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tVENDOR UNITS\tVENDOR USD\tLEDGER REQS\tREPORTED USD\tDORANG USD")
	rows := map[string]*[2]any{}
	var names []string
	if v != nil {
		for i := range v.models {
			m := &v.models[i]
			if rows[m.model] == nil {
				rows[m.model] = &[2]any{}
				names = append(names, m.model)
			}
			rows[m.model][0] = m
		}
	}
	for i := range ledger {
		m := &ledger[i]
		if rows[m.UpstreamModel] == nil {
			rows[m.UpstreamModel] = &[2]any{}
			names = append(names, m.UpstreamModel)
		}
		rows[m.UpstreamModel][1] = m
	}
	sort.Strings(names)
	for _, n := range names {
		vm, _ := rows[n][0].(*vendorModel)
		lm, _ := rows[n][1].(*store.ModelSpend)
		cols := []string{n, "-", "-", "-", "-", "-"}
		if vm != nil {
			cols[1] = strings.Join(vm.units, " + ")
			cols[2] = usd(vm.usd)
		}
		if lm != nil {
			cols[3] = fmt.Sprint(lm.Requests)
			if lm.ReportedRequests > 0 {
				cols[4] = nanoUSD(lm.ReportedNano)
			}
			cols[5] = nanoUSD(lm.CostNano)
		}
		fmt.Fprintln(tw, strings.Join(cols, "\t"))
	}
	tw.Flush()
	fmt.Fprintln(w)
}

func usd(v float64) string { return fmt.Sprintf("%.9f", v) }

func nanoUSD(n int64) string {
	sign := ""
	if n < 0 {
		sign, n = "-", -n
	}
	return fmt.Sprintf("%s%d.%09d", sign, n/1e9, n%1e9)
}

// monthRange is the UTC calendar month named by YYYY-MM, or the one now is in.
func monthRange(period string, now time.Time) (time.Time, time.Time, error) {
	var start time.Time
	if period == "" {
		start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	} else {
		t, err := time.Parse("2006-01", period)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("--period %q is not YYYY-MM", period)
		}
		start = t.UTC()
	}
	return start, start.AddDate(0, 1, 0), nil
}
