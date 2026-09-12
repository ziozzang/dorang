package backend

import (
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/wire/bedrock"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// bedrockAdapter speaks Amazon Bedrock's Converse API, signed with SigV4.
//
// The request and response are internal/wire/bedrock's; what is Bedrock's
// alone is the route — `/model/{modelId}/converse`, or `/converse-stream` for
// a streamed call, the model in the path — and the credential, which is not a
// header a caller sets but a SIGNATURE over the whole request. So this adapter
// does not implement [adapter.credential] the way the others do: it implements
// [requestSigner], and [Backend.send] signs the built request after the body
// and headers exist, because SigV4 covers all three.
//
// region and the access key id are provider settings (`params.region`,
// `params.access_key_id`); the secret access key is the credential, and an
// optional session token is `params.session_token`. Not measured against a
// live account — the operator has none — and the signer is pinned to AWS's own
// published test vector instead (sigv4_test.go).
type bedrockAdapter struct{}

func (bedrockAdapter) endpoint(p *Provider, op Operation, model string, stream bool) (string, error) {
	if op != OpChat {
		return "", noOperation("bedrock", op,
			"this adapter serves the Converse API only; embeddings and image models on Bedrock are "+
				"separate request shapes")
	}
	method := "/converse"
	if stream {
		method = "/converse-stream"
	}
	// The model id is opaque and may carry a version qualifier with a colon
	// ("...claude-3:0") or be a full ARN with slashes; both are encoded with
	// AWS's own rules so the path the request carries is the path the
	// signature will canonicalise to (see [awsURIEncode]).
	return joinVersioned(p.base, "", "") + "/model/" + awsURIEncode(model, true) + method, nil
}

// credential is a no-op: this surface authenticates by signature, applied in
// [Backend.send] through [requestSigner]. It exists so bedrockAdapter
// satisfies [adapter]; the secret it would set is instead the signing key.
func (bedrockAdapter) credential(string, http.Header) {}

func (bedrockAdapter) headers(http.Header) {}

func (bedrockAdapter) encode(x *exchange) ([]byte, error) {
	return bedrock.MarshalRequest(x.req, &bedrock.EncodeOptions{
		Capabilities:     x.capabilities(),
		Loss:             x.loss,
		DefaultMaxTokens: x.call.DefaultMaxTokens,
	})
}

func (bedrockAdapter) decode(body []byte, x *exchange) (*decoded, error) {
	if !bedrock.IsResponse(body) {
		return nil, errNotAResponse
	}
	resp, err := bedrock.DecodeResponse(body, &bedrock.DecodeOptions{Model: x.call.Model})
	if err != nil {
		return nil, err
	}
	return &decoded{resp: resp}, nil
}

func (bedrockAdapter) source(r io.Reader, x *exchange) (eventSource, error) {
	return &bedrockSource{d: bedrock.NewStreamDecoder(r, &bedrock.DecodeOptions{Model: x.call.Model})}, nil
}

// signRequest signs the built request with SigV4. It is [requestSigner], read
// by [Backend.send]; secret is the AWS secret access key resolved from the
// credential table.
func (bedrockAdapter) signRequest(req *http.Request, payload []byte, secret string, p *Provider, now time.Time) error {
	if p.bedrockAccessKeyID == "" {
		return errBedrockNeedsAccessKey
	}
	region := p.bedrockRegion
	if region == "" {
		region = regionFromHost(req.URL.Host)
	}
	if region == "" {
		return errBedrockNeedsRegion
	}
	signV4(req, payload, p.bedrockAccessKeyID, secret, p.bedrockSessionToken, region, "bedrock", now)
	return nil
}

// regionFromHost reads the region from a bedrock-runtime host
// ("bedrock-runtime.us-east-1.amazonaws.com"), so a provider that names its
// base URL need not repeat the region in params.
func regionFromHost(host string) string {
	const prefix = "bedrock-runtime."
	if !strings.HasPrefix(host, prefix) {
		return ""
	}
	rest := host[len(prefix):]
	if i := strings.IndexByte(rest, '.'); i > 0 {
		return rest[:i]
	}
	return ""
}

// bedrockSource adapts the Converse stream decoder to [eventSource].
type bedrockSource struct{ d *bedrock.StreamDecoder }

func (s *bedrockSource) next() ([]canonical.StreamEvent, error) { return s.d.Next() }

// terminated reports the family's own end marker: messageStop, which the
// decoder records. Unlike the OpenAI [DONE] sentinel it is an EVENT the relay
// also turns into a stop, so reporting it here is belt-and-braces — a stream
// cut off before messageStop is truncated on both readings.
func (s *bedrockSource) terminated() bool { return s.d.Terminated() }

// checkBedrockParams requires an access key id on the bedrock kind (the region
// may come from the host) and refuses the AWS settings on any other kind.
func checkBedrockParams(s Spec, api catalog.API) error {
	if api == catalog.APIBedrock {
		if s.BedrockAccessKeyID == "" {
			return errBedrockNeedsAccessKey
		}
		return nil
	}
	for name, set := range map[string]bool{
		"region":        s.BedrockRegion != "",
		"access_key_id": s.BedrockAccessKeyID != "",
		"session_token": s.BedrockSessionToken != "",
	} {
		if set {
			return errParamOnWrongKind(name, s.Kind, string(api), "an AWS SigV4 signing input, which no other host takes")
		}
	}
	return nil
}
