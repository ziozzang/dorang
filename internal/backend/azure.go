package backend

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/ziozzang/dorang/pkg/catalog"
)

// azureAdapter is the OpenAI wire shape on an Azure OpenAI resource.
//
// # What differs, and only that
//
// The body is OpenAI's. Two things are Azure's: where the route lives and how
// the credential is spelled.
//
//   - The unified `v1` surface (GA on every resource since 2025) serves the
//     OpenAI routes under `/openai/v1` — `/openai/v1/chat/completions`,
//     `/openai/v1/embeddings`, `/openai/v1/responses` — with the deployment
//     name in the body's `model`, exactly where the OpenAI encoder already
//     puts the upstream model. That is the default here and needs no
//     version parameter.
//   - The legacy surface addresses the deployment in the PATH,
//     `/openai/deployments/{deployment}/chat/completions`, and requires an
//     `api-version` query parameter on every request. It is selected by
//     `params.api_version`, which is the one thing that surface needs and the
//     unified one does not; an operator who sets it is asking for the legacy
//     route by name.
//   - The credential is the `api-key` header. `Authorization: Bearer` is also
//     accepted by Azure for an Entra ID token, and an OAuth credential in the
//     table travels that way through [Applier] untouched; a static key goes
//     where Azure documents it.
//
// Everything else — encoding, decoding, the stream reader, the capability
// set, the tool-name registry — is the OpenAI adapter's, by embedding.
//
// Not measured against a live resource: the operator has none. The routes and
// header are Azure's published contract, and the tests pin them against a
// fake; a deployment that finds otherwise has the report to say so.
type azureAdapter struct{ openaiAdapter }

// azureUnifiedPrefix is the unified surface's mount, appended to a base that
// names the resource alone (`https://{resource}.openai.azure.com`). A base
// that already carries it, or any version segment, is left as it is.
const azureUnifiedPrefix = "/openai/v1"

func (a azureAdapter) endpoint(p *Provider, op Operation, model string, _ bool) (string, error) {
	suffix, ok := azureRoute(op)
	if !ok {
		return "", noOperation("azure-openai", op,
			"this route is not served on an Azure OpenAI resource in this build")
	}
	if v := p.azureAPIVersion; v != "" {
		// The legacy surface: deployment in the path, api-version on the query.
		// The deployment name is opaque and may carry characters a path
		// segment reserves, so it is escaped.
		base := trimBase(p.base)
		base = strings.TrimSuffix(base, azureUnifiedPrefix)
		return base + "/openai/deployments/" + url.PathEscape(model) + suffix +
			"?api-version=" + url.QueryEscape(v), nil
	}
	return joinVersioned(p.base, azureUnifiedPrefix, suffix), nil
}

// azureRoute names the route suffix each operation takes on either surface.
func azureRoute(op Operation) (string, bool) {
	switch op {
	case OpChat:
		return pathChatCompletions, true
	case OpResponses:
		// The Responses surface is served under the unified mount as well;
		// on the legacy surface it is not a per-deployment route, so the
		// chat route is used there exactly as the OpenAI adapter uses it.
		return pathChatCompletions, true
	case OpEmbeddings:
		return pathEmbeddings, true
	case OpCompletions:
		return pathCompletions, true
	case OpSpeech:
		return pathSpeech, true
	case OpTranscription:
		return pathTranscriptions, true
	case OpTranslation:
		return pathTranslations, true
	case OpImageGenerate:
		return pathImageGenerate, true
	case OpImageEdit:
		return pathImageEdit, true
	case OpImageVariation:
		return pathImageVariation, true
	}
	return "", false
}

// credential is the `api-key` header Azure documents for a static key.
func (azureAdapter) credential(secret string, h http.Header) { h.Set("api-key", secret) }

// checkAzureParams refuses `params.api_version` on any kind but Azure: only
// [azureAdapter] reads it, and a key that loads and changes nothing is CONFIG
// §23.2's defect.
func checkAzureParams(s Spec, api catalog.API) error {
	if s.AzureAPIVersion != "" && api != catalog.APIAzureOpenAI {
		return errParamOnWrongKind("api_version", s.Kind, string(api),
			"the Azure OpenAI legacy surface's mandatory query parameter, which no other host takes")
	}
	return nil
}
