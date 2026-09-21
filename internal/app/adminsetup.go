package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ziozzang/dorang/internal/admin"
	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/pkg/catalog"
	"gopkg.in/yaml.v3"
)

type adminSetup struct{ a *App }

func setupRevision(raw []byte) string { h := sha256.Sum256(raw); return hex.EncodeToString(h[:]) }
func (s *adminSetup) read() ([]byte, *config.Config, error) {
	if s.a.configPath == "" {
		return nil, s.a.Config(), nil
	}
	raw, err := config.ReadSetup(s.a.configPath)
	if err != nil {
		return nil, nil, err
	}
	cfg, err := config.LoadBytes(raw)
	return raw, cfg, err
}
func (s *adminSetup) Snapshot(ctx context.Context) (map[string]any, error) {
	raw, cfg, err := s.read()
	if err != nil {
		return nil, err
	}
	live := s.a.Config()
	providers := []map[string]any{}
	credentials := []map[string]any{}
	models := []map[string]any{}
	for _, p := range cfg.Providers {
		applied := false
		for _, q := range live.Providers {
			if p.Name == q.Name {
				applied = reflect.DeepEqual(p, q)
			}
		}
		providers = append(providers, map[string]any{"id": p.Name, "kind": p.Kind, "base_url": p.BaseURL, "applied": applied, "parameters": map[string]string{"api_version": p.Params.APIVersion, "project": p.Params.Project, "location": p.Params.Location, "region": p.Params.Region, "access_key_id": p.Params.AccessKeyID}})
	}
	for _, c := range cfg.Credentials {
		applied := false
		for _, q := range live.Credentials {
			if c.ID == q.ID {
				applied = reflect.DeepEqual(c, q)
			}
		}
		auth := c.Auth
		if auth == "" {
			auth = "key"
		}
		source, ref, format := "secret", "", "generic"
		if c.Key.Env != "" {
			source = "env"
			ref = c.Key.Env
		}
		if c.Key.File != "" {
			source = "file"
			ref = c.Key.File
		}
		if c.OAuth != nil {
			source = c.OAuth.Source
			format = c.OAuth.Format
			ref = c.OAuth.Path
			if source == "env" {
				ref = c.OAuth.EnvVar
			}
		}
		credentials = append(credentials, map[string]any{"id": c.ID, "provider": c.Provider, "auth": auth, "source": source, "reference": ref, "format": format, "applied": applied, "configured": true})
	}
	for _, m := range cfg.Models {
		for _, d := range m.Deployments {
			applied := false
			for _, lm := range live.Models {
				if lm.Name == m.Name {
					for _, ld := range lm.Deployments {
						if reflect.DeepEqual(d, ld) {
							applied = true
						}
					}
				}
			}
			models = append(models, map[string]any{"model": m.Name, "provider": d.Provider, "upstream": d.UpstreamModel, "credentials": d.Credentials, "enabled": d.Enabled == nil || *d.Enabled, "applied": applied, "weight": d.Weight, "priority": d.Priority})
		}
	}
	status := "applied"
	if !reflect.DeepEqual(cfg.Providers, live.Providers) || !reflect.DeepEqual(cfg.Credentials, live.Credentials) || !reflect.DeepEqual(cfg.Models, live.Models) {
		status = "pending"
	}
	if checkOAuthUnchanged(live, cfg) != nil {
		status = "restart_required"
	}
	kinds := []map[string]string{}
	if s.catalog() != nil {
		for _, k := range s.catalog().Kinds() {
			kd, _ := s.catalog().Kind(k)
			kinds = append(kinds, map[string]string{"id": k, "base_url": kd.BaseURL})
		}
	}
	return map[string]any{"writable": s.a.configPath != "" && s.a.reloadNow != nil, "revision": setupRevision(raw), "status": status, "providers": providers, "credentials": credentials, "models": models, "kinds": kinds}, nil
}

var setupName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_./:@+-]{0,191}$`)

func (s *adminSetup) Change(ctx context.Context, in admin.SetupChange) (map[string]any, error) {
	if s.a.configPath == "" || s.a.reloadNow == nil {
		return nil, admin.Unsupported("Configuration editing is not connected to a file.")
	}
	if !setupName.MatchString(in.ID) || len(in.Revision) != 64 {
		return nil, admin.SetupInvalid("A valid identifier and configuration revision are required.")
	}
	s.a.configWriteMu.Lock()
	defer s.a.configWriteMu.Unlock()
	var secretFile, status string
	var resultRevision string
	var before, after map[string]any
	prepared := false
	err := config.EditLocked(s.a.configPath, func(raw []byte) ([]byte, error) {
		if setupRevision(raw) != in.Revision {
			return nil, admin.SetupConflict("Configuration changed in another session. Refresh this page before saving again.")
		}
		cfg, err := config.LoadBytes(raw)
		if err != nil {
			return nil, err
		}
		before = setupAuditState(cfg, in)
		edit := config.SetupEdit{Create: !in.Edit, ID: in.ID, Fields: map[string]any{}}
		switch in.Action {
		case "provider":
			edit.Section = "providers"
			edit.IDKey = "name"
			if _, ok := s.catalog().Kind(in.Kind); !ok {
				return nil, admin.SetupInvalid("Select a supported provider kind.")
			}
			if in.BaseURL != "" {
				u, e := url.Parse(in.BaseURL)
				if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
					return nil, admin.SetupInvalid("Use an HTTP(S) endpoint without credentials, query parameters or a fragment.")
				}
			}
			// Switching adapter family can leave incompatible provider parameters behind.
			for _, p := range cfg.Providers {
				if p.Name == in.ID && p.Kind != in.Kind {
					return nil, admin.SetupInvalid("Create a new connection to change the provider kind.")
				}
			}
			edit.Parameters = map[string]any{}
			for k, v := range in.Parameters {
				switch k {
				case "api_version", "project", "location", "region", "access_key_id":
					if len(v) > 256 {
						return nil, admin.SetupInvalid("Provider parameters are too long.")
					}
					edit.Parameters[k] = v
				default:
					return nil, admin.SetupInvalid("Unsupported provider parameter.")
				}
			}
			edit.Fields["kind"] = in.Kind
			edit.Fields["base_url"] = strings.TrimRight(in.BaseURL, "/")
		case "credential":
			edit.Section = "credentials"
			edit.IDKey = "id"
			found := false
			for _, p := range cfg.Providers {
				if p.Name == in.Provider {
					found = true
				}
			}
			if !found {
				return nil, admin.SetupInvalid("Select an existing provider connection.")
			}
			var old *config.Credential
			for i := range cfg.Credentials {
				if cfg.Credentials[i].ID == in.ID {
					old = &cfg.Credentials[i]
				}
			}
			if old != nil && old.Provider != in.Provider {
				return nil, admin.SetupInvalid("Create a new account to change its provider connection.")
			}
			if in.Auth != "key" && in.Auth != "oauth" {
				return nil, admin.SetupInvalid("Select API key/token or OAuth token store.")
			}
			edit.Fields["provider"] = in.Provider
			if in.Source == "keep" {
				if old == nil {
					return nil, admin.SetupInvalid("A new account requires authentication.")
				}
				break
			}
			edit.Fields["auth"] = in.Auth
			edit.Remove = []string{"key", "key_env", "key_file", "key_ref", "oauth"}
			ref := strings.TrimSpace(in.Reference)
			if in.Source == "secret" {
				if strings.TrimSpace(in.Secret) == "" || len(in.Secret) > 64<<10 {
					return nil, admin.SetupInvalid("Enter a non-empty key or token store of at most 64 KiB.")
				}
				if in.Auth == "oauth" && !json.Valid([]byte(in.Secret)) {
					return nil, admin.SetupInvalid("The OAuth token store must be valid JSON.")
				}
				dir := filepath.Join(filepath.Dir(s.a.configPath), "secrets")
				if err := os.MkdirAll(dir, 0700); err != nil {
					return nil, err
				}
				f, err := os.CreateTemp(dir, "credential-*")
				if err != nil {
					return nil, err
				}
				secretFile = f.Name()
				if _, err = f.WriteString(strings.TrimSpace(in.Secret)); err == nil {
					err = f.Sync()
				}
				closeErr := f.Close()
				if err != nil {
					return nil, err
				}
				if closeErr != nil {
					return nil, closeErr
				}
				ref = secretFile
			} else if in.Source != "file" && in.Source != "env" {
				return nil, admin.SetupInvalid("Select a secret value, file or environment variable.")
			}
			if ref == "" {
				return nil, admin.SetupInvalid("An authentication source is required.")
			}
			if in.Auth == "key" {
				field := "key_file"
				if in.Source == "env" {
					field = "key_env"
				}
				edit.Fields[field] = ref
			} else {
				source := in.Source
				if source == "secret" {
					source = "file"
				}
				format := in.Format
				if format == "" {
					format = "generic"
				}
				oauth := map[string]any{}
				if old != nil && old.OAuth != nil { // Retain existing refresh configuration when rotating the token source.
					// Encode YAML, since its tags define the configuration vocabulary.
					oauth = oauthSetupFields(old.OAuth)
				}
				oauth["source"] = source
				oauth["format"] = format
				delete(oauth, "path")
				delete(oauth, "env_var")
				delete(oauth, "command")
				if source == "env" {
					oauth["env_var"] = ref
				} else {
					oauth["path"] = ref
				}
				edit.Fields["oauth"] = oauth
			}
		case "model":
			if !setupName.MatchString(in.Model) || !setupName.MatchString(in.Upstream) {
				return nil, admin.SetupInvalid("Enter the service model name and upstream model ID.")
			}
			if in.Weight < 0 || in.Priority < 0 {
				return nil, admin.SetupInvalid("Weight and priority must be non-negative.")
			}
			matched := false
			for _, c := range cfg.Credentials {
				if c.ID == in.Credential && c.Provider == in.Provider {
					matched = true
				}
			}
			if !matched {
				return nil, admin.SetupInvalid("Select an account belonging to this provider.")
			}
			for _, m := range cfg.Models {
				if m.Name == in.Model {
					for _, d := range m.Deployments {
						for _, c := range d.Credentials {
							if d.Provider == in.Provider && d.UpstreamModel == in.Upstream && c == in.Credential {
								return nil, admin.ErrConflict
							}
						}
					}
				}
			}
			edit.Section = "deployments"
			edit.IDKey = ""
			edit.Create = true
			edit.Model = in.Model
			edit.Fields = map[string]any{"provider": in.Provider, "upstream_model": in.Upstream, "credentials": []string{in.Credential}, "weight": in.Weight, "priority": in.Priority, "enabled": in.Enabled}
		default:
			return nil, admin.SetupInvalid("Unknown setup operation.")
		}
		next, err := config.EditSetup(raw, edit)
		if err != nil {
			return nil, admin.SetupInvalid("The item already exists or was removed. Refresh the setup page.")
		}
		candidate, err := config.LoadBytes(next)
		if err != nil {
			return nil, admin.SetupInvalid("Configuration validation failed. Check the provider endpoint, credential source and model settings.")
		}
		if _, err := newUpstreamTable(candidate, s.catalog()); err != nil {
			return nil, admin.SetupInvalid("The provider adapter could not be configured. Check its kind and required parameters.")
		}
		status = "saved"
		if checkOAuthUnchanged(s.a.Config(), candidate) != nil {
			status = "restart_required"
		}
		after = setupAuditState(candidate, in)
		prepared = true
		resultRevision = setupRevision(next)
		return next, nil
	})
	if err != nil {
		if secretFile != "" && !prepared {
			_ = os.Remove(secretFile)
		}
		return nil, err
	}
	if status != "restart_required" {
		if err := s.a.reloadNow(); err != nil {
			status = "apply_failed"
		} else {
			status = "applied"
		}
	}
	return map[string]any{"id": in.ID, "operation": in.Action, "revision": resultRevision, "status": status, "secret_updated": secretFile != "", "before": before, "after": after}, nil
}

// Discovery is an explicit operator action. It never sends inference requests,
// follows redirects, or forwards credentials to a URL supplied in the probe.
func (s *adminSetup) Discover(ctx context.Context, id string) (map[string]any, error) {
	cfg := s.a.Config()
	var cred *config.Credential
	var provider *config.Provider
	for i := range cfg.Credentials {
		if cfg.Credentials[i].ID == id {
			cred = &cfg.Credentials[i]
		}
	}
	if cred == nil {
		return nil, admin.SetupInvalid("Apply the account configuration before checking its connection.")
	}
	for i := range cfg.Providers {
		if cfg.Providers[i].Name == cred.Provider {
			provider = &cfg.Providers[i]
		}
	}
	if provider == nil {
		return nil, admin.ErrNotFound
	}
	kd, ok := s.catalog().Kind(provider.Kind)
	if !ok {
		return nil, admin.ErrUnsupported
	}
	base := provider.BaseURL
	if base == "" {
		base = kd.BaseURL
	}
	// Other vendor families can be configured manually without claiming that a
	// generic model-list route verifies their account permissions.
	if kd.API != catalog.APIOpenAIChat && kd.API != catalog.APIAnthropicMessages {
		return map[string]any{"status": "manual", "models": []string{}, "credential": id}, nil
	}
	endpoint := strings.TrimRight(base, "/") + "/models"
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, admin.SetupInvalid("The configured endpoint is invalid.")
	}
	if cred.IsOAuth() {
		if s.a.OAuth == nil {
			return nil, admin.ErrUnsupported
		}
		c, ok := s.a.OAuth.Credential(id)
		if !ok {
			return nil, admin.ErrNotFound
		}
		if err := c.Apply(req.Header); err != nil {
			return map[string]any{"status": "authentication_failed", "models": []string{}, "credential": id}, nil
		}
	} else if kd.API == catalog.APIAnthropicMessages {
		key, _ := cred.Key.Value()
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		key, _ := cred.Key.Value()
		req.Header.Set("Authorization", "Bearer "+key)
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return map[string]any{"status": "unreachable", "models": []string{}, "credential": id}, nil
	}
	defer res.Body.Close()
	out := map[string]any{"status": "ready", "http_status": res.StatusCode, "credential": id, "models": []string{}}
	if res.StatusCode != 200 {
		out["status"] = "unavailable"
		if res.StatusCode == 401 || res.StatusCode == 403 {
			out["status"] = "authentication_failed"
		}
		return out, nil
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, (2<<20)+1))
	if err != nil || len(raw) > 2<<20 {
		out["status"] = "invalid_response"
		return out, nil
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &payload) != nil || payload.Data == nil {
		out["status"] = "invalid_response"
		return out, nil
	}
	ids := []string{}
	seen := map[string]bool{}
	for _, m := range payload.Data {
		if len(ids) >= 1000 {
			break
		}
		if m.ID != "" && len(m.ID) <= 192 && !seen[m.ID] {
			seen[m.ID] = true
			ids = append(ids, m.ID)
		}
	}
	sort.Strings(ids)
	out["models"] = ids
	return out, nil
}

func oauthSetupFields(o *config.OAuth) map[string]any {
	raw, _ := yaml.Marshal(o)
	m := map[string]any{}
	_ = yaml.Unmarshal(raw, &m)
	return m
}

func (s *adminSetup) catalog() *catalog.Catalog { return s.a.dispatch.state().catalog }

func setupAuditState(cfg *config.Config, in admin.SetupChange) map[string]any {
	switch in.Action {
	case "provider":
		for _, p := range cfg.Providers {
			if p.Name == in.ID {
				return map[string]any{"id": p.Name, "kind": p.Kind, "base_url": p.BaseURL}
			}
		}
	case "credential":
		for _, c := range cfg.Credentials {
			if c.ID == in.ID {
				return map[string]any{"id": c.ID, "provider": c.Provider, "auth": c.Auth, "source": c.Key.Source(), "oauth": c.IsOAuth()}
			}
		}
	case "model":
		for _, m := range cfg.Models {
			if m.Name == in.Model {
				return map[string]any{"model": m.Name, "deployment_count": len(m.Deployments)}
			}
		}
	}
	return nil
}
