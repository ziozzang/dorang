package admin

import (
	"errors"
	"net/http"
	"strings"
)

// budgetView is a budget and its consumption on the wire.
type budgetView struct {
	// BudgetID is the addressable name, "<kind>:<id>". dorang has no reusable
	// named-budget object — the ceiling lives on the subject row and the
	// consumption in budget_state (§9.2) — so the id is derived from the
	// subject rather than being a key of its own.
	BudgetID    string `json:"budget_id"`
	SubjectKind string `json:"subject_kind"`
	SubjectID   string `json:"subject_id"`

	MaxBudget  *Money `json:"max_budget"`
	SoftBudget *Money `json:"soft_budget"`
	Period     string `json:"budget_duration"`

	Spend    Money `json:"spend"`
	Reserved Money `json:"reserved"`
	// Remaining is nil when no ceiling is configured: "unlimited" and "zero
	// left" are different answers and a number cannot express the first.
	Remaining *Money `json:"remaining"`

	PeriodStart   Stamp `json:"period_start"`
	PeriodEnd     Stamp `json:"period_end"`
	ReservedUntil Stamp `json:"reserved_until"`
	UpdatedAt     Stamp `json:"updated_at"`
}

func viewBudget(b Budget) budgetView {
	v := budgetView{
		BudgetID:      budgetID(b.Subject),
		SubjectKind:   b.Subject.Kind,
		SubjectID:     b.Subject.ID,
		MaxBudget:     moneyPtr(b.MaxBudgetNano),
		SoftBudget:    moneyPtr(b.SoftBudgetNano),
		Period:        b.Period,
		Spend:         Money(b.SpentNano),
		Reserved:      Money(b.ReservedNano),
		PeriodStart:   Stamp(b.PeriodStart),
		PeriodEnd:     Stamp(b.PeriodEnd),
		ReservedUntil: Stamp(b.ReservedUntil),
		UpdatedAt:     Stamp(b.UpdatedAt),
	}
	if b.MaxBudgetNano != nil {
		// Reserved counts against the ceiling: a hold is money that is not
		// available even though it has not been spent (§6.4).
		rem := Money(*b.MaxBudgetNano - b.SpentNano - b.ReservedNano)
		v.Remaining = &rem
	}
	return v
}

func budgetID(s BudgetSubject) string { return s.Kind + ":" + s.ID }

// budgetSubjectKinds is the vocabulary of §6.4. It is closed, because a subject
// kind the rest of the system does not enforce is a budget that never applies.
var budgetSubjectKinds = map[string]bool{
	"key": true, "user": true, "team": true, "credential": true, "global": true,
}

type budgetSpec struct {
	BudgetID    string `json:"budget_id"`
	SubjectKind string `json:"subject_kind"`
	SubjectID   string `json:"subject_id"`

	MaxBudget  *Money  `json:"max_budget"`
	SoftBudget *Money  `json:"soft_budget"`
	Period     *string `json:"budget_duration"`
}

// subject resolves the budget's subject from either form.
func (s *budgetSpec) subject(r *httpQuery) (BudgetSubject, error) {
	kind := strings.TrimSpace(s.SubjectKind)
	id := strings.TrimSpace(s.SubjectID)
	raw := strings.TrimSpace(s.BudgetID)
	if raw == "" && r != nil {
		raw = r.get("budget_id")
	}
	if kind == "" && r != nil {
		kind = r.get("subject_kind")
	}
	if id == "" && r != nil {
		id = r.get("subject_id")
	}
	if kind == "" && raw != "" {
		k, rest, ok := strings.Cut(raw, ":")
		if !ok {
			return BudgetSubject{}, badRequest(
				"budget_id must be \"<subject_kind>:<subject_id>\", for example \"team:eng\"; " +
					"dorang budgets attach to a subject rather than being reusable named objects").
				withParam("budget_id")
		}
		kind, id = strings.TrimSpace(k), strings.TrimSpace(rest)
	}
	if kind == "" {
		return BudgetSubject{}, badRequest("subject_kind is required").withParam("subject_kind")
	}
	if !budgetSubjectKinds[kind] {
		return BudgetSubject{}, badRequest("subject_kind %q is not one of key, user, team, credential, global", kind).
			withParam("subject_kind")
	}
	if kind == "global" {
		id = ""
	} else if id == "" {
		return BudgetSubject{}, badRequest("subject_id is required for subject_kind %q", kind).withParam("subject_id")
	}
	return BudgetSubject{Kind: kind, ID: id}, nil
}

// httpQuery is a tiny adapter so that a spec can fall back to query parameters
// without every resolver taking an *http.Request.
type httpQuery struct{ c *call }

func (q *httpQuery) get(name string) string { return queryString(q.c.r, name) }

// POST /budget/new and POST /budget/update
//
// They differ in one check: new refuses to overwrite an existing ceiling, and
// update refuses to invent one. Making them the same handler with a flag is
// what keeps the two from drifting apart in what they validate.
func budgetSet(mustExist bool) handler {
	return func(c *call) error {
		bs, err := c.a.budgets()
		if err != nil {
			return err
		}
		if err := c.requireAudit(); err != nil {
			return err
		}
		var spec budgetSpec
		if err := decodeBody(c.w, c.r, &spec); err != nil {
			return err
		}
		sub, err := spec.subject(&httpQuery{c})
		if err != nil {
			return err
		}

		existing, err := bs.GetBudget(c.ctx(), sub)
		switch {
		case err == nil:
		case errors.Is(err, ErrNotFound):
			existing = Budget{Subject: sub}
		default:
			return err
		}
		configured := existing.MaxBudgetNano != nil || existing.SoftBudgetNano != nil
		if mustExist && !configured {
			return notFound("budget", budgetID(sub))
		}
		if !mustExist && configured {
			return newFault(http.StatusConflict, CodeConflict, typeInvalidRequest,
				"budget %q already exists; use /budget/update", budgetID(sub))
		}
		before := viewBudget(existing)

		next := existing
		if spec.MaxBudget != nil {
			next.MaxBudgetNano = nanoPtr(spec.MaxBudget)
		}
		if spec.SoftBudget != nil {
			next.SoftBudgetNano = nanoPtr(spec.SoftBudget)
		}
		if spec.Period != nil {
			next.Period = strings.TrimSpace(*spec.Period)
		}
		if next.MaxBudgetNano == nil && next.SoftBudgetNano == nil {
			return badRequest("max_budget or soft_budget is required").withParam("max_budget")
		}
		if next.MaxBudgetNano != nil && *next.MaxBudgetNano < 0 {
			return badRequest("max_budget must not be negative").withParam("max_budget")
		}
		if next.SoftBudgetNano != nil && next.MaxBudgetNano != nil &&
			*next.SoftBudgetNano > *next.MaxBudgetNano {
			return badRequest("soft_budget must not exceed max_budget").withParam("soft_budget")
		}
		if err := bs.SetBudget(c.ctx(), next); err != nil {
			return err
		}

		stored, err := bs.GetBudget(c.ctx(), sub)
		if err != nil {
			return err
		}
		after := viewBudget(stored)
		action := "budget.new"
		if mustExist {
			action = "budget.update"
		}
		var beforeState any
		if configured {
			beforeState = before
		}
		if err := c.recordAudit(action, "budget", budgetID(sub), beforeState, after); err != nil {
			return err
		}
		writeJSON(c.w, c.r, http.StatusOK, map[string]any{"budget": after})
		return nil
	}
}

// GET|POST /budget/info
func (c *call) budgetInfo() error {
	bs, err := c.a.budgets()
	if err != nil {
		return err
	}
	var spec budgetSpec
	if err := decodeOptionalBody(c.w, c.r, &spec); err != nil {
		return err
	}
	sub, err := spec.subject(&httpQuery{c})
	if err != nil {
		return err
	}
	b, err := bs.GetBudget(c.ctx(), sub)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return notFound("budget", budgetID(sub))
		}
		return err
	}
	writeJSON(c.w, c.r, http.StatusOK, map[string]any{"budget": viewBudget(b)})
	return nil
}

// POST /budget/delete
func (c *call) budgetDelete() error {
	bs, err := c.a.budgets()
	if err != nil {
		return err
	}
	if err := c.requireAudit(); err != nil {
		return err
	}
	var spec budgetSpec
	if err := decodeBody(c.w, c.r, &spec); err != nil {
		return err
	}
	sub, err := spec.subject(&httpQuery{c})
	if err != nil {
		return err
	}
	b, err := bs.GetBudget(c.ctx(), sub)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return notFound("budget", budgetID(sub))
		}
		return err
	}
	before := viewBudget(b)
	if err := bs.ClearBudget(c.ctx(), sub); err != nil {
		return err
	}
	// Recorded state matters here: clearing a budget removes the ceiling, it
	// does not remove the spend, and an operator reading the trail later needs
	// to see which of the two happened.
	if err := c.recordAudit("budget.delete", "budget", budgetID(sub), before,
		map[string]any{"max_budget": nil, "soft_budget": nil, "spend_preserved": true}); err != nil {
		return err
	}
	writeJSON(c.w, c.r, http.StatusOK, map[string]any{
		"deleted":   true,
		"budget_id": budgetID(sub),
	})
	return nil
}

// GET|POST /budget/list
func (c *call) budgetList() error {
	bs, err := c.a.budgets()
	if err != nil {
		return err
	}
	o, err := c.listOptions()
	if err != nil {
		return err
	}
	list, err := bs.ListBudgets(c.ctx(), o)
	if err != nil {
		return err
	}
	out := make([]budgetView, 0, len(list))
	for _, b := range list {
		out = append(out, viewBudget(b))
	}
	writeJSON(c.w, c.r, http.StatusOK, map[string]any{
		"budgets": out,
		"limit":   o.Limit,
		"offset":  o.Offset,
		"count":   len(out),
	})
	return nil
}
