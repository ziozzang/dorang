package router

import (
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/capacity"
)

// The two capacity refusals of COMPATIBILITY §11.2, asserted where they are
// raised: the status AND the code, together.
//
// They used to answer 503 with `no_capacity` / `no_candidate` — dorang's own
// internal words for §11.2's `capacity_unavailable` and `no_healthy_deployment`,
// at a status §11.2 argues against by name. A client cannot branch on a
// vocabulary it was never given, and every SDK reads a 503 as a dead gateway and
// stops retrying where it would have backed off on a 429.

// TestNoHealthyDeploymentIsA429 covers both spellings of "nothing is serving".
func TestNoHealthyDeploymentIsA429(t *testing.T) {
	cfg := Config{Groups: []Group{{Name: "m", Deployments: []Deployment{
		dep("d1", "p1", "openai", "u1"),
		dep("d2", "p2", "openai", "u2"),
	}}}}
	h := newHarness(t, cfg, harnessOpts{})
	h.health.MarkUnavailable("d1", 24*time.Hour)
	h.health.MarkUnavailable("d2", 24*time.Hour)

	e := h.routeErr(Request{Model: "m"})
	if e.Code != CodeNoCandidate {
		t.Errorf("code %q, want %q", e.Code, CodeNoCandidate)
	}
	if e.Code != "no_healthy_deployment" {
		t.Errorf("§11.2 spells this row %q, got %q", "no_healthy_deployment", e.Code)
	}
	if e.Status != 429 {
		t.Errorf("status %d, want 429: §11.2 says a capacity condition is back-pressure, "+
			"and an SDK reads a 503 as a dead gateway", e.Status)
	}
}

// TestCapacityUnavailableIsA429 is the other neighbour of the same row: every
// candidate is up and every candidate is full.
func TestCapacityUnavailableIsA429(t *testing.T) {
	cfg := Config{Groups: []Group{{Name: "m", Deployments: []Deployment{
		dep("d1", "p1", "openai", "u1"),
	}}}}
	cc := capacity.Config{SweepInterval: -1,
		Models: []capacity.ModelLimit{{Provider: "p1", Model: "u1", Max: 1}}}
	h := newHarness(t, cfg, harnessOpts{capacity: cc})

	held := h.route(Request{Model: "m"})
	defer h.ok(held)

	e := h.routeErr(Request{Model: "m"})
	if e.Code != CodeNoCapacity {
		t.Errorf("code %q, want %q", e.Code, CodeNoCapacity)
	}
	if e.Code != "capacity_unavailable" {
		t.Errorf("§11.2 spells this row %q, got %q", "capacity_unavailable", e.Code)
	}
	if e.Status != 429 {
		t.Errorf("status %d, want 429 (§11.2)", e.Status)
	}
}

// TestNoEligibleDeploymentIsA429 covers the other raise site of the same code:
// the filter left nothing, for a reason none of the more specific refusals
// claims. The two sites answer the same way, which is the point — a client
// cannot see which of dorang's internal paths ran out first.
func TestNoEligibleDeploymentIsA429(t *testing.T) {
	h := newHarness(t, Config{Groups: []Group{{Name: "m"}}}, harnessOpts{})
	e := h.routeErr(Request{Model: "m"})
	if e.Code != "no_healthy_deployment" || e.Status != 429 {
		t.Errorf("status %d code %q, want 429 no_healthy_deployment (§11.2)", e.Status, e.Code)
	}
}

// TestUnknownModelIsA404WithTheCompat11Code pins the third row a router refusal
// produces, so the §11.1 worked example has a source as well as a rendering.
func TestUnknownModelIsA404WithTheCompat11Code(t *testing.T) {
	h := newHarness(t, Config{Groups: []Group{{Name: "m",
		Deployments: []Deployment{dep("d1", "p1", "openai", "u1")}}}}, harnessOpts{})
	e := h.routeErr(Request{Model: "never-configured"})
	if e.Status != 404 || e.Code != "model_not_found" {
		t.Errorf("status %d code %q, want 404 model_not_found", e.Status, e.Code)
	}
}
