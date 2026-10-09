package probe

import (
	"fmt"
	"strings"
	"testing"
)

func TestOpenRouterCreditsDecode(t *testing.T) {
	r, err := openrouterSource{}.decode([]byte(`{"data":{"total_credits":40,"total_usage":20.660322588}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.balances) != 1 {
		t.Fatalf("balances = %+v", r.balances)
	}
	b := r.balances[0]
	if b.RemainingNano != 19_339_677_412 || b.TotalNano != 40_000_000_000 || b.Currency != "USD" || !b.Available {
		t.Errorf("balance = %+v", b)
	}
	if _, err := (openrouterSource{}).decode([]byte(`{"data":{}}`)); err == nil {
		t.Error("a body without the two figures decoded")
	}
	if r, err := (openrouterSource{}).decode([]byte(`{"data":{"total_credits":1e1,"total_usage":2.5e-1}}`)); err != nil ||
		r.balances[0].RemainingNano != 9_750_000_000 {
		t.Errorf("exponent form: %+v, %v", r, err)
	}
}

// TestDeepInfraChecklistDecodesOnlyNumbers feeds the checklist with its personal
// members and requires that none of them survives into the reading.
func TestDeepInfraChecklistDecodesOnlyNumbers(t *testing.T) {
	body := `{"email":true,"billing_address":true,"billing_address_info":{"name":"A Person","line1":"1 Street"},
		"payment_method":true,"payment_method_info":{"last4":"4242"},"suspended":false,"overdue_invoices":0.0,
		"stripe_balance":-20.0,"recent":0.26,"limit":5.0,"topup":false,"billing_type":"prepaid","scoped_credits":[]}`
	r, err := deepinfraSource{}.decode([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if dump := fmt.Sprintf("%+v", r); strings.Contains(dump, "Person") || strings.Contains(dump, "4242") ||
		strings.Contains(dump, "Street") {
		t.Fatalf("personal data reached the reading: %s", dump)
	}
	if len(r.windows) != 1 {
		t.Fatalf("windows = %+v", r.windows)
	}
	w := r.windows[0]
	if !w.AlertOnly || w.Mapped || w.Used != 260_000_000 || w.Limit != 5_000_000_000 || w.UsedPercent != 5.2 {
		t.Errorf("spend-limit window = %+v", w)
	}
	if _, ok := w.providerWindow(); ok {
		t.Error("an alert-only window reached quota")
	}
	if len(r.balances) != 1 || r.balances[0].RemainingNano != 19_740_000_000 || !r.balances[0].Available {
		t.Errorf("credit = %+v, want 20 less the month's 0.26", r.balances)
	}
	// No limit and no credit: nothing to report is an error, not an empty success.
	if _, err := (deepinfraSource{}).decode([]byte(`{"recent":1,"limit":0,"stripe_balance":3}`)); err == nil {
		t.Error("a checklist with neither figure decoded")
	}
}

func TestAnAllowanceCannotRefileAnAlertOnlyWindow(t *testing.T) {
	p := &Prober{cfg: Config{Allowances: []Allowance{{Label: "spend_limit:monthly"}}}}
	ws := p.applyAllowances([]Window{{Label: "spend_limit:monthly", AlertOnly: true}})
	if ws[0].Mapped {
		t.Error("an allowance re-filed an alert-only window under a quota key")
	}
}
