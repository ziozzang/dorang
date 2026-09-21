package admin

import (
	"context"
	"net/http"
)

// SetupManager owns the desired configuration and its application, not a
// parallel database registry. Its snapshots and results must be secret-free.
type SetupManager interface {
	Snapshot(context.Context) (map[string]any, error)
	Change(context.Context, SetupChange) (map[string]any, error)
	Discover(context.Context, string) (map[string]any, error)
}
type SetupChange struct {
	Parameters map[string]string `json:"parameters"`
	Action     string            `json:"action"`
	Revision   string            `json:"revision"`
	ID         string            `json:"id"`
	Provider   string            `json:"provider"`
	Kind       string            `json:"kind"`
	BaseURL    string            `json:"base_url"`
	Auth       string            `json:"auth"`
	Source     string            `json:"source"`
	Secret     string            `json:"secret"`
	Reference  string            `json:"reference"`
	Format     string            `json:"format"`
	Model      string            `json:"model"`
	Upstream   string            `json:"upstream"`
	Credential string            `json:"credential"`
	Weight     int               `json:"weight"`
	Priority   int               `json:"priority"`
	Enabled    bool              `json:"enabled"`
	Edit       bool              `json:"edit"`
}

func (c *call) setupSnapshot() error {
	if err := c.requireGlobal("upstream setup"); err != nil {
		return err
	}
	if c.a.cfg.Setup == nil {
		return dependencyOff("setup manager", "upstream setup")
	}
	result, err := c.a.cfg.Setup.Snapshot(c.ctx())
	if err != nil {
		return err
	}
	writeJSON(c.w, c.r, http.StatusOK, result)
	return nil
}
func (c *call) setupChange() error {
	if err := c.requireGlobal("upstream setup"); err != nil {
		return err
	}
	if c.a.cfg.Setup == nil {
		return dependencyOff("setup manager", "upstream setup")
	}
	if err := c.requireAudit(); err != nil {
		return err
	}
	var in SetupChange
	if err := decodeBody(c.w, c.r, &in); err != nil {
		return err
	}
	result, err := c.a.cfg.Setup.Change(c.ctx(), in)
	if err != nil {
		return err
	}
	// Explicit allow-list: never serialize the submitted form into an audit row.
	before := result["before"]
	after := any(result)
	if state, ok := result["after"]; ok {
		after = map[string]any{"configuration": state, "status": result["status"], "revision": result["revision"], "secret_updated": result["secret_updated"]}
	}
	objectID := in.ID
	if in.Action == "model" {
		objectID = in.Model + "|" + in.Provider + "|" + in.Upstream + "|" + in.Credential
	}
	if err := c.recordAudit("setup."+in.Action, "configuration", objectID, before, after); err != nil {
		return err
	}
	writeJSON(c.w, c.r, http.StatusOK, result)
	return nil
}
func (c *call) setupDiscover() error {
	if err := c.requireGlobal("upstream model discovery"); err != nil {
		return err
	}
	if c.a.cfg.Setup == nil {
		return dependencyOff("setup manager", "upstream discovery")
	}
	if err := c.requireAudit(); err != nil {
		return err
	}
	var in struct {
		Credential string `json:"credential"`
	}
	if err := decodeBody(c.w, c.r, &in); err != nil {
		return err
	}
	result, err := c.a.cfg.Setup.Discover(c.ctx(), in.Credential)
	if err != nil {
		return err
	}
	if err := c.recordAudit("setup.discover", "credential", in.Credential, nil, map[string]any{"status": result["status"]}); err != nil {
		return err
	}
	writeJSON(c.w, c.r, http.StatusOK, result)
	return nil
}

func SetupInvalid(message string) error { return badRequest("%s", message) }

func SetupConflict(message string) error {
	return newFault(http.StatusConflict, CodeConflict, typeInvalidRequest, "%s", message)
}
