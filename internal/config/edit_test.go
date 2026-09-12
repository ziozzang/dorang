package config

import (
	"strings"
	"testing"
)

const editSample = `# top comment, must survive
server:
  listen: ":4100"
providers:
  - {name: p1, kind: openai, base_url: "https://x.invalid"}
models:
  - name: chat
    deployments:
      # keep this comment on the first deployment
      - provider: p1
        upstream_model: vendor/chat-2026
      - provider: p1
        upstream_model: vendor/chat-mini
  - name: embed
    deployments:
      - {provider: p1, upstream_model: vendor/embed}
`

// The surgical edit sets exactly the named deployment's flag and preserves the
// operator's comments and the other entries — the whole reason for editing the
// node tree rather than re-serializing a decoded Config.
func TestSetDeploymentEnabledIsSurgical(t *testing.T) {
	out, err := SetDeploymentEnabled([]byte(editSample), "chat", "p1", "vendor/chat-mini", false)
	if err != nil {
		t.Fatalf("SetDeploymentEnabled: %v", err)
	}
	s := string(out)

	// Comments survive.
	for _, c := range []string{"top comment, must survive", "keep this comment on the first deployment"} {
		if !strings.Contains(s, c) {
			t.Errorf("a comment was lost: %q\n%s", c, s)
		}
	}

	// The edited result parses and carries the flag only where set.
	cfg, err := LoadBytes(out)
	if err != nil {
		t.Fatalf("the edited config does not load: %v\n%s", err, s)
	}
	var chat *Model
	for i := range cfg.Models {
		if cfg.Models[i].Name == "chat" {
			chat = &cfg.Models[i]
		}
	}
	if chat == nil || len(chat.Deployments) != 2 {
		t.Fatalf("chat model shape changed: %+v", chat)
	}
	for _, d := range chat.Deployments {
		switch d.UpstreamModel {
		case "vendor/chat-mini":
			if d.Enabled == nil || *d.Enabled {
				t.Errorf("chat-mini was not disabled: %v", d.Enabled)
			}
		case "vendor/chat-2026":
			if d.Enabled != nil {
				t.Errorf("chat-2026 was touched: %v", d.Enabled)
			}
		}
	}

	// Re-enabling flips it back.
	out2, err := SetDeploymentEnabled(out, "chat", "p1", "vendor/chat-mini", true)
	if err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	cfg2, _ := LoadBytes(out2)
	for _, m := range cfg2.Models {
		if m.Name != "chat" {
			continue
		}
		for _, d := range m.Deployments {
			if d.UpstreamModel == "vendor/chat-mini" && (d.Enabled == nil || !*d.Enabled) {
				t.Errorf("re-enable did not set enabled true: %v", d.Enabled)
			}
		}
	}
}

func TestSetDeploymentEnabledRefusesUnknownAndAmbiguous(t *testing.T) {
	if _, err := SetDeploymentEnabled([]byte(editSample), "chat", "p1", "nope", false); err == nil {
		t.Error("an unknown deployment was accepted")
	}
	dup := editSample + `  - name: dup
    deployments:
      - {provider: p1, upstream_model: same}
      - {provider: p1, upstream_model: same}
`
	if _, err := SetDeploymentEnabled([]byte(dup), "dup", "p1", "same", false); err == nil {
		t.Error("an ambiguous deployment triple was accepted rather than refused")
	}
}
