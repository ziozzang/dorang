package backend

import (
	"errors"

	json "github.com/ziozzang/dorang/internal/safejson"
)

// Apply after encoding: unknown vendor fields survive conversion and a caller
// cannot replace an operator's provider policy. Replace each complete value;
// merging the caller's provider object could retain conflicting routing keys.
// Providers with no overrides retain their original bytes and behavior.
func (p *Provider) applySetParams(body []byte, contentType string) ([]byte, error) {
	if len(p.setParams) == 0 {
		return body, nil
	}
	if contentType != "" && contentType != "application/json" {
		return nil, errors.New("params.set requires a JSON request")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil || object == nil {
		return nil, errors.New("params.set requires a JSON object")
	}
	for name, value := range p.setParams {
		object[name] = value
	}
	return json.Marshal(object)
}
