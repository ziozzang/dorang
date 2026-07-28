package app

import (
	"github.com/ziozzang/dorang/internal/router"
	"github.com/ziozzang/dorang/internal/server"
	"github.com/ziozzang/dorang/internal/tokenest"
)

// estimate is this call's prompt size, for the context-window filter (§10.5a)
// and the input half of the budget hold (§6.4).
//
// It is structural whenever dorang decoded the request into a typed form, which
// is every surface except the two it relays. The distinction matters most where
// the request carries something that is not text: an image, a PDF, an uploaded
// recording. Those are body bytes, and counting body bytes charges a 2 MB photo
// ~900,000 tokens against a real cost near 1,600 — a number large enough that no
// deployment's window admits it, so the request is refused everywhere rather
// than routed to a larger model. internal/tokenest counts each of them by its
// own rule instead.
//
// The byte fallback is reached only by the paths with no typed request at all:
// embeddings, which has no neutral representation and is relayed with just the
// model name replaced, and anything that decoded to nothing.
func (c *call) estimate() tokenest.Estimate {
	switch {
	case c.creq != nil:
		return tokenest.Request(c.creq)
	case c.rerankReq != nil:
		return tokenest.Rerank(c.rerankReq)
	case c.modReq != nil:
		return tokenest.Moderation(c.modReq)
	case c.speechReq != nil:
		return tokenest.Speech(c.speechReq)
	case c.transReq != nil:
		return tokenest.Transcription(c.transReq)
	case c.imageReq != nil:
		return tokenest.Image(c.imageReq)
	}
	return tokenest.Bytes(c.body)
}

// upstreamCause classifies one upstream failure into a fail-back class.
//
// [router.Classify] reads the status line and stops there, on purpose: it never
// looks inside a body. That leaves a hole exactly where §10.5a needs a signal,
// because an upstream context overflow is a 400 whose only distinguishing mark
// IS the body — and a deployment that declares no context window has no
// pre-computed overflow either, so for the self-hosted engines of docs/VLLM.md
// and docs/SGLANG.md the body is the only signal that exists at all.
//
// [server.Normalize] has already decoded that body into the one envelope dorang
// speaks, across all five upstream shapes. This consults it. An unrecognised
// 400 classifies as before — CauseNone, terminal — so the only behaviour that
// changes is the one that was missing.
func upstreamCause(status int, e *server.Error) router.Cause {
	if cause := router.Classify(status, nil); cause != router.CauseNone {
		return cause
	}
	if e == nil {
		return router.CauseNone
	}
	return router.ClassifyBody(status, e.Code, e.Message)
}
