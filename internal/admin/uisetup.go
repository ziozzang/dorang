package admin

import (
	"net/url"
	"strconv"
)

func init() {
	uiActions["setup_discover"] = uiAction{api: "/admin/setup/discover", verb: "check upstream connection", past: "checked", jsonResponse: true, needs: func(s *uiServer) bool { return s.api.cfg.Setup != nil }, body: func(f url.Values) (any, error) { return map[string]any{"credential": f.Get("credential")}, nil }}
	uiActions["setup_save"] = uiAction{api: "/admin/setup/change", verb: "save upstream configuration", past: "saved", needs: func(s *uiServer) bool { return s.api.cfg.Setup != nil }, body: func(f url.Values) (any, error) {
		n := func(key string) (int, error) {
			if f.Get(key) == "" {
				return 0, nil
			}
			return strconv.Atoi(f.Get(key))
		}
		weight, err := n("weight")
		if err != nil {
			return nil, SetupInvalid("Weight must be a whole number.")
		}
		priority, err := n("priority")
		if err != nil {
			return nil, SetupInvalid("Priority must be a whole number.")
		}
		parameters := map[string]string{}
		for _, key := range []string{"api_version", "project", "location", "region", "access_key_id"} {
			if values, ok := f[key]; ok && len(values) > 0 {
				parameters[key] = values[0]
			}
		}
		return SetupChange{Parameters: parameters, Action: f.Get("operation"), Revision: f.Get("revision"), ID: f.Get("id"), Provider: f.Get("provider"), Kind: f.Get("kind"), BaseURL: f.Get("base_url"), Auth: f.Get("auth"), Source: f.Get("source"), Secret: f.Get("secret"), Reference: f.Get("reference"), Format: f.Get("format"), Model: f.Get("model"), Upstream: f.Get("upstream"), Credential: f.Get("credential"), Weight: weight, Priority: priority, Enabled: f.Get("enabled") == "true", Edit: f.Get("edit") == "true"}, nil
	}, notice: func(_ url.Values, res map[string]any) string {
		switch res["status"] {
		case "applied":
			return "Configuration applied on this node. Other nodes apply it through their configuration watchers."
		case "restart_required":
			return "Configuration saved. OAuth account changes require a rolling restart before they become active."
		default:
			return "Configuration saved but not applied. Review the pending configuration before continuing."
		}
	}}
}

func setupReturn(base string, f url.Values) string {
	q := url.Values{}
	anchor := "service-models"
	switch f.Get("operation") {
	case "provider":
		q.Set("provider", f.Get("id"))
		anchor = "accounts"
		if f.Get("edit") == "true" {
			anchor = "providers"
		}
	case "credential":
		q.Set("provider", f.Get("provider"))
		q.Set("credential", f.Get("id"))
		if f.Get("edit") == "true" {
			anchor = "accounts"
		}
	case "model":
		q.Set("provider", f.Get("provider"))
		q.Set("credential", f.Get("credential"))
	}
	return base + "/setup?" + q.Encode() + "#" + anchor
}
