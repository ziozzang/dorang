package admin

import (
	"net/http"
	"strings"
	"testing"
)

func newDeployment(t *testing.T, h *harness, group, provider, upstream string) string {
	t.Helper()
	body := h.expectStatus(h.do(http.MethodPost, "/model/new", map[string]any{
		"model_name": group,
		"dorang_params": map[string]any{
			"provider": provider, "model": upstream, "weight": 3, "rpm": 600,
		},
	}), http.StatusOK)
	return body["model"].(map[string]any)["id"].(string)
}

func TestDeploymentLifecycle(t *testing.T) {
	h := newHarness(t)
	id := newDeployment(t, h, "chat-fast", "openai-compatible", "vendor/model-x:2026-07")

	e, _ := h.store.lastAudit()
	if e.Action != "model.new" || e.ObjectKind != "deployment" {
		t.Fatalf("audit = %+v", e)
	}

	body := h.expectStatus(h.do(http.MethodGet, "/model/info?id="+id, nil), http.StatusOK)
	data := body["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("data = %v", data)
	}
	d := data[0].(map[string]any)
	params := d["dorang_params"].(map[string]any)
	// The model name is opaque and must survive byte for byte (§2.1).
	if params["model"] != "vendor/model-x:2026-07" {
		t.Fatalf("upstream model was altered: %v", params["model"])
	}
	if _, ok := d["litellm_params"]; !ok {
		t.Error("the legacy parameter alias is missing, so an existing script cannot read it")
	}

	h.expectStatus(h.do(http.MethodPost, "/model/update", map[string]any{
		"id": id, "dorang_params": map[string]any{"weight": 7}, "enabled": false,
	}), http.StatusOK)
	e, _ = h.store.lastAudit()
	if !strings.Contains(e.Before, `"weight":3`) || !strings.Contains(e.After, `"weight":7`) {
		t.Fatalf("update audit does not carry both weights: %s / %s", e.Before, e.After)
	}

	h.expectStatus(h.do(http.MethodPost, "/model/delete", map[string]any{"id": id}), http.StatusOK)
	e, _ = h.store.lastAudit()
	if e.After != "" {
		t.Errorf("a deletion recorded an after state: %q", e.After)
	}
	h.expectFault(h.do(http.MethodGet, "/model/info?id="+id, nil), http.StatusNotFound, CodeNotFound)
}

func TestDeploymentRequiresGroupProviderAndModel(t *testing.T) {
	h := newHarness(t)
	for _, body := range []map[string]any{
		{},
		{"model_name": "g"},
		{"model_name": "g", "dorang_params": map[string]any{"provider": "p"}},
	} {
		h.expectFault(h.do(http.MethodPost, "/model/new", body),
			http.StatusBadRequest, CodeInvalidRequest)
	}
}

func TestModelGroupInfoFoldsDeploymentsAndAliases(t *testing.T) {
	h := newHarness(t)
	newDeployment(t, h, "chat", "prov-a", "a/model")
	newDeployment(t, h, "chat", "prov-b", "b/model")
	id := newDeployment(t, h, "embed", "prov-a", "a/embed")
	h.store.aliases = []Alias{{Alias: "chat-latest", ModelGroup: "chat"}}

	// A disabled deployment still counts as configured but not as servable.
	h.expectStatus(h.do(http.MethodPost, "/model/update",
		map[string]any{"id": id, "enabled": false}), http.StatusOK)

	body := h.expectStatus(h.do(http.MethodGet, "/model_group/info", nil), http.StatusOK)
	groups := body["data"].([]any)
	if len(groups) != 2 {
		t.Fatalf("groups = %v", groups)
	}
	chat := groups[0].(map[string]any)
	if chat["model_group"] != "chat" {
		t.Fatalf("groups are not sorted: %v", chat)
	}
	if len(chat["providers"].([]any)) != 2 {
		t.Errorf("providers = %v", chat["providers"])
	}
	if chat["aliases"].([]any)[0] != "chat-latest" {
		t.Errorf("aliases = %v", chat["aliases"])
	}
	embed := groups[1].(map[string]any)
	if embed["deployments"] != 1.0 || embed["enabled_deployments"] != 0.0 {
		t.Errorf("a group whose only deployment is disabled must not look servable: %v", embed)
	}
}

func TestModelGroupInfoByName(t *testing.T) {
	h := newHarness(t)
	newDeployment(t, h, "chat", "prov-a", "a/model")

	body := h.expectStatus(h.do(http.MethodGet, "/model_group/info?model_group=chat", nil), http.StatusOK)
	if len(body["data"].([]any)) != 1 {
		t.Fatalf("data = %v", body["data"])
	}
	h.expectFault(h.do(http.MethodGet, "/model_group/info?model_group=nope", nil),
		http.StatusNotFound, CodeNotFound)
}

func TestDeploymentParamsMustBeAJSONObject(t *testing.T) {
	h := newHarness(t)
	h.expectFault(h.do(http.MethodPost, "/model/new", map[string]any{
		"model_name":    "g",
		"dorang_params": map[string]any{"provider": "p", "model": "m", "params": []int{1, 2}},
	}), http.StatusBadRequest, CodeInvalidRequest)
}
