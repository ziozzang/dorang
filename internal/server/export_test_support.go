package server

import "net/http"

// NewTestRequest builds a *Request outside the serving path.
//
// It exists for internal/app's tests, which need to drive the dispatcher's
// decode step without standing up an HTTP server — the fields it fills are the
// ones Server.serve fills, and no others, so a test cannot accidentally depend
// on state the real path would not have set.
//
// It is not in an _test.go file because the consumer is a different package.
// Nothing in the serving path calls it.
func NewTestRequest(r *http.Request, rt *Route, p Principal, model string, body []byte) *Request {
	rq := &Request{
		HTTP:      r,
		Method:    r.Method,
		Path:      r.URL.Path,
		Route:     rt,
		Principal: p,
		Model:     model,
	}
	rq.body.buf = body
	rq.Body = &rq.body
	return rq
}
