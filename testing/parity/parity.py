#!/usr/bin/env python3
"""Differential parity harness: LiteLLM (reference) vs dorang (candidate).

Sends the same request to both gateways and compares the *structure* of the
answers -- field presence, types, SSE framing, usage accounting, error
envelopes. Model text differs between calls by nature, so nothing here compares
bytes of generated content; it compares shape and invariants.

Usage
-----
    set -a; . ~/env; set +a          # LITELLM_OPENAI_BASE_URL / _API_KEY
    export DORANG_BASE_URL=http://127.0.0.1:4199
    export DORANG_API_KEY="$DORANG_MASTER_KEY"

    ./parity.py                      # the default suite
    ./parity.py --list               # what it would run, spending nothing
    ./parity.py --only errors        # one group
    ./parity.py --json out.json      # machine-readable results

Groups: models, chat, chat-stream, embeddings, rerank, audio, images,
messages, errors.

Cost
----
Every generative case uses a two-token prompt and max_tokens=8, one call per
gateway. Image *generation* is not run by default (--include-image opts in);
the images endpoint is otherwise exercised through an argument-error probe that
costs nothing. See REPORT.md for the full skip list.

Stdlib only -- no third-party imports, so it runs wherever python3 does.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request
import uuid

# --------------------------------------------------------------------------
# HTTP
# --------------------------------------------------------------------------


class Answer:
    """One gateway's answer to one request."""

    def __init__(self, status, headers, body, elapsed, transport_error=None):
        self.status = status
        self.headers = {k.lower(): v for k, v in (headers or {}).items()}
        self.body = body or b""
        self.elapsed = elapsed
        self.transport_error = transport_error

    @property
    def ctype(self):
        return self.headers.get("content-type", "").split(";")[0].strip().lower()

    def json(self):
        try:
            return json.loads(self.body.decode("utf-8"))
        except Exception:
            return None

    def __repr__(self):
        return "<Answer %s %s %dB>" % (self.status, self.ctype, len(self.body))


class Gateway:
    def __init__(self, name, base_url, api_key):
        self.name = name
        self.base = base_url.rstrip("/")
        self.key = api_key

    def resolve(self, path):
        """Join a case's path onto this gateway's base without doubling `/v1`.

        The two gateways are addressed differently and both spellings are
        correct: LITELLM_OPENAI_BASE_URL already carries `/v1`, while dorang is
        addressed at its bare listener and routes `/v1/...` and `/...` both. A
        case that writes `/v1/audio/speech` would otherwise reach
        `.../v1/v1/audio/speech` on one of them and get a 404 that looks like an
        unimplemented route -- which is exactly the false finding this method
        exists to prevent.
        """
        if self.base.endswith("/v1") and path.startswith("/v1/"):
            return self.base + path[len("/v1"):]
        return self.base + path

    def call(self, path, body=None, headers=None, method=None, timeout=180,
             key=NotImplemented, content_type="application/json"):
        url = self.resolve(path)
        hdrs = {}
        auth = self.key if key is NotImplemented else key
        if auth is not None:
            hdrs["Authorization"] = "Bearer " + auth
        data = None
        if body is not None:
            if isinstance(body, (bytes, bytearray)):
                data = bytes(body)
            else:
                data = json.dumps(body).encode("utf-8")
            if content_type:
                hdrs["Content-Type"] = content_type
        hdrs.update(headers or {})
        req = urllib.request.Request(
            url, data=data, headers=hdrs, method=method or ("POST" if data else "GET"))
        t0 = time.time()
        try:
            with urllib.request.urlopen(req, timeout=timeout) as r:
                return Answer(r.status, dict(r.headers), r.read(), time.time() - t0)
        except urllib.error.HTTPError as e:
            return Answer(e.code, dict(e.headers or {}), e.read(), time.time() - t0)
        except Exception as e:  # transport: DNS, TLS, reset, timeout
            return Answer(None, {}, b"", time.time() - t0,
                          transport_error="%s: %s" % (type(e).__name__, e))


# --------------------------------------------------------------------------
# Structural comparison
# --------------------------------------------------------------------------

def typename(v):
    if v is None:
        return "null"
    if isinstance(v, bool):
        return "bool"
    if isinstance(v, int):
        return "number"
    if isinstance(v, float):
        return "number"
    if isinstance(v, str):
        return "string"
    if isinstance(v, list):
        return "array"
    if isinstance(v, dict):
        return "object"
    return type(v).__name__


def flatten(value, prefix=""):
    """JSON value -> {path: typename}. Array elements collapse to one '[]' slot.

    Collapsing means a 3-element array of identical objects yields the same map
    as a 1-element one: the harness is comparing shape, and a differing element
    *count* is checked separately by the invariants that care about it.
    """
    out = {}
    if isinstance(value, dict):
        for k, v in value.items():
            p = prefix + ("." if prefix else "") + k
            out[p] = typename(v)
            out.update(flatten(v, p))
    elif isinstance(value, list):
        p = prefix + "[]"
        seen = set()
        for v in value:
            out.update(flatten(v, p))
            seen.add(typename(v))
        if seen:
            out[p] = "|".join(sorted(seen))
    return out


# Paths whose absence or type change breaks a real client rather than a linter.
# Everything not listed is reported at "low".
CRITICAL = (
    "usage", "usage.prompt_tokens", "usage.completion_tokens", "usage.total_tokens",
    "usage.input_tokens", "usage.output_tokens",
    "choices[].finish_reason", "choices[].message.content", "choices[].message.role",
    "choices[].delta", "choices[].index",
    "id", "object", "model", "created",
    "data[].embedding[]", "data[].index", "data[].object",
    "error", "error.message", "error.type", "error.code", "error.param",
    "results[].index", "results[].relevance_score",
    "content[].text", "stop_reason", "role", "type",
)


def severity(path):
    return "high" if path in CRITICAL else "low"


def compare_shape(ref, cand):
    """Diff two decoded JSON bodies. Returns a list of divergence dicts."""
    if ref is None and cand is None:
        return []
    if ref is None or cand is None:
        return [{"kind": "not-json", "path": "", "ref": typename(ref),
                 "cand": typename(cand), "sev": "high"}]
    a, b = flatten(ref), flatten(cand)
    diffs = []
    for p in sorted(set(a) - set(b)):
        diffs.append({"kind": "missing-in-dorang", "path": p,
                      "ref": a[p], "cand": None, "sev": severity(p)})
    for p in sorted(set(b) - set(a)):
        diffs.append({"kind": "extra-in-dorang", "path": p,
                      "ref": None, "cand": b[p], "sev": "low"})
    for p in sorted(set(a) & set(b)):
        if a[p] != b[p]:
            # null vs a concrete type is the one type difference that is
            # routinely load-bearing: a client reading .usage.total_tokens off a
            # null usage object raises where the reference did not.
            sev = "high" if (a[p] == "null" or b[p] == "null") else severity(p)
            diffs.append({"kind": "type-differs", "path": p,
                          "ref": a[p], "cand": b[p], "sev": sev})
    return diffs


# --------------------------------------------------------------------------
# SSE
# --------------------------------------------------------------------------

def parse_sse(raw):
    """Return (frames, notes). A frame is (field, value) in wire order."""
    frames, notes = [], []
    text = raw.decode("utf-8", "replace")
    if "\r\n" in text:
        notes.append("CRLF line endings in SSE body")
    for block in text.split("\n\n"):
        if not block.strip():
            continue
        for line in block.split("\n"):
            if not line:
                continue
            if line.startswith(":"):
                frames.append(("comment", line[1:]))
            elif ":" in line:
                f, _, v = line.partition(":")
                frames.append((f.strip(), v.lstrip()))
            else:
                notes.append("SSE line with no field separator: %r" % line[:60])
    return frames, notes


def sse_report(raw, family="openai"):
    """Structural facts about an SSE body, per COMPATIBILITY §1 and §6."""
    frames, notes = parse_sse(raw)
    data = [v for f, v in frames if f == "data"]
    events = [v for f, v in frames if f == "event"]
    chunks = []
    for d in data:
        if d.strip() == "[DONE]":
            continue
        try:
            chunks.append(json.loads(d))
        except Exception:
            notes.append("undecodable data frame: %r" % d[:80])
    rep = {
        "frames": len(frames),
        "data_frames": len(data),
        "event_lines": len(events),
        "event_types": sorted(set(events)),
        "comment_lines": sum(1 for f, _ in frames if f == "comment"),
        "id_lines": sum(1 for f, _ in frames if f == "id"),
        "terminates_with_done": bool(data) and data[-1].strip() == "[DONE]",
        "done_count": sum(1 for d in data if d.strip() == "[DONE]"),
        "chunks": len(chunks),
        "notes": notes,
    }
    if family == "openai":
        ids = {c.get("id") for c in chunks if isinstance(c, dict)}
        created = {c.get("created") for c in chunks if isinstance(c, dict)}
        models = {c.get("model") for c in chunks if isinstance(c, dict)}
        objects = {c.get("object") for c in chunks if isinstance(c, dict)}
        finishes = [ch.get("finish_reason")
                    for c in chunks if isinstance(c, dict)
                    for ch in (c.get("choices") or []) if isinstance(ch, dict)]
        rep.update({
            "id_pinned": len(ids) <= 1,
            "created_pinned": len(created) <= 1,
            "models": sorted(m for m in models if m is not None),
            "objects": sorted(o for o in objects if o is not None),
            "finish_reasons": [f for f in finishes if f is not None],
            "has_usage_chunk": any(c.get("usage") for c in chunks if isinstance(c, dict)),
            "usage_chunk_choices": next(
                (typename(c.get("choices")) + ":" + str(len(c.get("choices") or []))
                 for c in chunks if isinstance(c, dict) and c.get("usage")), None),
            "chunk_shape": sorted({k for c in chunks if isinstance(c, dict) for k in c}),
        })
    return rep


def compare_sse(ref_raw, cand_raw, family="openai"):
    a, b = sse_report(ref_raw, family), sse_report(cand_raw, family)
    diffs = []
    # Framing invariants that hold regardless of what the model said.
    keys_high = ["terminates_with_done", "done_count", "id_pinned", "created_pinned",
                 "objects", "event_types", "id_lines", "chunk_shape",
                 "has_usage_chunk", "usage_chunk_choices"]
    keys_low = ["comment_lines"]
    for k in keys_high + keys_low:
        if k in a and a.get(k) != b.get(k):
            diffs.append({"kind": "sse", "path": k, "ref": a.get(k), "cand": b.get(k),
                          "sev": "high" if k in keys_high else "low"})
    # finish_reason: the *set* matters, the count does not (token counts vary).
    if set(a.get("finish_reasons") or []) != set(b.get("finish_reasons") or []):
        diffs.append({"kind": "sse", "path": "finish_reasons",
                      "ref": sorted(set(a.get("finish_reasons") or [])),
                      "cand": sorted(set(b.get("finish_reasons") or [])),
                      "sev": "high"})
    if b.get("notes"):
        diffs.append({"kind": "sse", "path": "notes", "ref": a.get("notes"),
                      "cand": b.get("notes"), "sev": "high"})
    return diffs, a, b


# --------------------------------------------------------------------------
# Cases
# --------------------------------------------------------------------------

# A sample across provider families, not across models. Where several ids sit
# on one upstream family (glm-4.7/glm-5/glm-5.1; kimi-k2.5/k2.6; the four
# nim:*), one is exercised and the rest are named as skipped in REPORT.md.
CHAT_SAMPLE = [
    ("gpt-oss:20b", "self-hosted / ollama-shaped, ':' in the id"),
    ("glm-5", "zhipu GLM"),
    ("zai:glm-5-turbo", "z.ai, ':' namespace prefix"),
    ("deepseek-v3.2", "deepseek, '.' in the id"),
    ("nim:deepseek-v4-flash", "NVIDIA NIM, ':' namespace prefix"),
    ("gemini-3-flash-preview", "google gemini"),
    ("kimi-k2.5", "moonshot"),
    ("minimax-m2.7", "minimax"),
    ("qwen3-coder-next", "qwen"),
    ("local", "LiteLLM-side alias"),
]

EMBED_SAMPLE = [
    ("jina-embeddings-v5-text-nano", "jina"),
    ("embeddinggemma-300m", "self-hosted"),
    ("pplx-embed-v1-0.6b", "perplexity"),
    ("llama-nemotron-embed-vl-1b-v2", "nvidia"),
]

RERANK_MODEL = "jina-reranker-v3"
SPEECH_MODEL = "tts-1"
TRANSCRIBE_MODEL = "whisper-1"
IMAGE_MODEL = "gpt-image-1.5"

PROMPT = [{"role": "user", "content": "hi"}]
MAXTOK = 8


def chat_body(model, stream=False, usage=False):
    b = {"model": model, "messages": PROMPT, "max_tokens": MAXTOK, "temperature": 0}
    if stream:
        b["stream"] = True
        if usage:
            b["stream_options"] = {"include_usage": True}
    return b


class Case:
    """One comparison. run(gw) issues the request; compare() diffs the pair."""

    group = "misc"

    def __init__(self, name, detail=""):
        self.name = name
        self.detail = detail

    def run(self, gw):
        raise NotImplementedError

    def compare(self, ref, cand):
        return compare_shape(ref.json(), cand.json())

    def extras(self, ref, cand):
        return []


class JSONCase(Case):
    def __init__(self, group, name, path, body=None, detail="", method=None,
                 key=NotImplemented, headers=None, content_type="application/json",
                 timeout=180):
        Case.__init__(self, name, detail)
        self.group = group
        self.path = path
        self.body = body
        self.method = method
        self.key = key
        self.headers = headers
        self.content_type = content_type
        self.timeout = timeout

    def run(self, gw):
        return gw.call(self.path, self.body, headers=self.headers, method=self.method,
                       key=self.key, content_type=self.content_type,
                       timeout=self.timeout)


class ChatCase(JSONCase):
    def extras(self, ref, cand):
        out = []
        for label, a in (("litellm", ref), ("dorang", cand)):
            d = a.json() or {}
            u = d.get("usage") or {}
            if u and all(k in u for k in ("prompt_tokens", "completion_tokens", "total_tokens")):
                if u["prompt_tokens"] + u["completion_tokens"] != u["total_tokens"]:
                    out.append({"kind": "invariant", "path": "usage.total_tokens",
                                "ref": label, "cand": "prompt+completion != total",
                                "sev": "high"})
        # The model field must echo what the client asked for.
        want = (self.body or {}).get("model")
        for label, a in (("litellm", ref), ("dorang", cand)):
            got = (a.json() or {}).get("model")
            if a.status == 200 and got is not None and got != want:
                out.append({"kind": "restamp", "path": "model", "ref": want,
                            "cand": "%s returned %r" % (label, got), "sev": "med"})
        return out


class StreamCase(JSONCase):
    def compare(self, ref, cand):
        if ref.status != 200 or cand.status != 200:
            return compare_shape(ref.json(), cand.json())
        diffs, a, b = compare_sse(ref.body, cand.body)
        self._rep = (a, b)
        return diffs

    def extras(self, ref, cand):
        out = []
        for label, a in (("litellm", ref), ("dorang", cand)):
            if a.status == 200 and a.ctype not in ("text/event-stream",):
                out.append({"kind": "content-type", "path": "Content-Type",
                            "ref": "text/event-stream",
                            "cand": "%s: %s" % (label, a.ctype), "sev": "high"})
        return out


class BinaryCase(JSONCase):
    """Audio speech: the answer is bytes, and the bytes must survive intact."""

    def compare(self, ref, cand):
        diffs = []
        if ref.status != 200 or cand.status != 200:
            return compare_shape(ref.json(), cand.json())
        if ref.ctype != cand.ctype:
            diffs.append({"kind": "content-type", "path": "Content-Type",
                          "ref": ref.ctype, "cand": cand.ctype, "sev": "high"})
        # An mp3 frame header, or an ID3 tag, must be at the head of both.
        for label, a in (("litellm", ref), ("dorang", cand)):
            head = a.body[:3]
            if not (head[:3] == b"ID3" or (len(a.body) > 1 and a.body[0] == 0xFF)):
                diffs.append({"kind": "binary", "path": "magic",
                              "ref": "ID3 or 0xFF sync",
                              "cand": "%s: %r" % (label, head), "sev": "high"})
        if ref.body and cand.body:
            ratio = len(cand.body) / len(ref.body)
            if not (0.2 < ratio < 5.0):
                diffs.append({"kind": "binary", "path": "length",
                              "ref": len(ref.body), "cand": len(cand.body),
                              "sev": "med"})
        if not cand.body:
            diffs.append({"kind": "binary", "path": "length", "ref": len(ref.body),
                          "cand": 0, "sev": "high"})
        return diffs


class ModelsCase(JSONCase):
    def compare(self, ref, cand):
        diffs = compare_shape(ref.json(), cand.json())
        a = {m["id"] for m in (ref.json() or {}).get("data", [])}
        b = {m["id"] for m in (cand.json() or {}).get("data", [])}
        for mid in sorted(a - b):
            diffs.append({"kind": "model-missing", "path": mid, "ref": "present",
                          "cand": "absent", "sev": "high"})
        for mid in sorted(b - a):
            diffs.append({"kind": "model-extra", "path": mid, "ref": "absent",
                          "cand": "present", "sev": "high"})
        return diffs

    def extras(self, ref, cand):
        out = []
        ra = {m["id"]: m for m in (ref.json() or {}).get("data", [])}
        ca = {m["id"]: m for m in (cand.json() or {}).get("data", [])}
        for field in ("created", "owned_by", "object"):
            rv = {m.get(field) for m in ra.values()}
            cv = {m.get(field) for m in ca.values()}
            if rv != cv:
                out.append({"kind": "models-field", "path": field,
                            "ref": sorted(map(str, rv)), "cand": sorted(map(str, cv)),
                            "sev": "med" if field != "object" else "high"})
        return out


def build_cases(args):
    cases = []

    # 1. GET /models -- the list itself, and the retrieve route beside it.
    cases.append(ModelsCase("models", "GET /models", "/models"))
    cases.append(JSONCase("models", "GET /models/{id} (colon id)",
                          "/models/" + "gpt-oss%3A20b",
                          detail="a ':' in a path segment must not be split"))

    # 2. chat/completions, non-streaming then streaming.
    for model, why in CHAT_SAMPLE[:args.chat_limit]:
        cases.append(ChatCase("chat", "POST /chat/completions %s" % model,
                              "/chat/completions", chat_body(model), detail=why))
    for model, why in CHAT_SAMPLE[:args.chat_limit]:
        cases.append(StreamCase("chat-stream", "POST /chat/completions %s [stream]" % model,
                                "/chat/completions", chat_body(model, stream=True, usage=True),
                                detail=why + "; stream_options.include_usage"))

    # 3. embeddings -- two inputs, so count and ordering are observable.
    for model, why in EMBED_SAMPLE[:args.embed_limit]:
        cases.append(EmbedCase("embeddings", "POST /embeddings %s" % model, "/embeddings",
                               {"model": model, "input": ["alpha", "beta gamma"]},
                               detail=why))

    # 4. rerank
    cases.append(RerankCase("rerank", "POST /rerank %s" % RERANK_MODEL, "/rerank",
                            {"model": RERANK_MODEL, "query": "capital of France",
                             "documents": ["Berlin is in Germany.",
                                           "Paris is the capital of France.",
                                           "Tokyo is in Japan."],
                             "top_n": 3},
                            detail="index ordering and score field naming"))

    # 5. audio + image
    cases.append(BinaryCase("audio", "POST /v1/audio/speech %s" % SPEECH_MODEL,
                            "/v1/audio/speech",
                            {"model": SPEECH_MODEL, "input": "hi", "voice": "alloy",
                             "response_format": "mp3"},
                            detail="binary answer must survive the gateway intact"))
    if args.include_transcribe:
        cases.append(TranscribeCase("audio", "POST /v1/audio/transcriptions %s"
                                    % TRANSCRIBE_MODEL, "/v1/audio/transcriptions",
                                    detail="multipart upload; text field shape"))
    cases.append(JSONCase("images", "POST /v1/images/generations (argument error)",
                          "/v1/images/generations", {"model": IMAGE_MODEL},
                          detail="prompt omitted on purpose: exercises the image route "
                                 "and its error envelope without generating an image"))
    if args.include_image:
        cases.append(JSONCase("images", "POST /v1/images/generations %s" % IMAGE_MODEL,
                              "/v1/images/generations",
                              {"model": IMAGE_MODEL, "prompt": "a red square", "n": 1},
                              detail="COSTS MONEY -- opted in with --include-image",
                              timeout=300))

    # 6. /v1/messages -- the Anthropic family envelope.
    cases.append(MessagesCase("messages", "POST /v1/messages %s" % args.messages_model,
                              "/v1/messages",
                              {"model": args.messages_model, "max_tokens": MAXTOK,
                               "messages": PROMPT},
                              detail="Anthropic-family shape on an OpenAI-shaped backend"))

    # 7. errors -- these matter more than the happy paths.
    cases.append(JSONCase("errors", "unknown model (chat)", "/chat/completions",
                          {"model": "no-such-model-" + uuid.uuid4().hex[:8],
                           "messages": PROMPT, "max_tokens": MAXTOK},
                          detail="COMPATIBILITY §11.2: 404 / invalid_request_error / "
                                 "model_not_found"))
    cases.append(JSONCase("errors", "unknown model (embeddings)", "/embeddings",
                          {"model": "no-such-model-" + uuid.uuid4().hex[:8],
                           "input": "x"},
                          detail="same condition, second family"))
    cases.append(JSONCase("errors", "bad api key", "/chat/completions",
                          chat_body(CHAT_SAMPLE[0][0]), key="sk-definitely-not-valid",
                          detail="401 / authentication_error / invalid_api_key"))
    cases.append(JSONCase("errors", "no auth header", "/chat/completions",
                          chat_body(CHAT_SAMPLE[0][0]), key=None,
                          detail="401, and no upstream call"))
    cases.append(JSONCase("errors", "bad api key on GET /models", "/models",
                          key="sk-definitely-not-valid",
                          detail="the list endpoint's auth failure shape"))
    cases.append(JSONCase("errors", "malformed JSON body", "/chat/completions",
                          b'{"model": "x", "messages": [',
                          detail="400 / invalid_request_error / invalid_request"))
    cases.append(JSONCase("errors", "unknown parameter", "/chat/completions",
                          dict(chat_body(CHAT_SAMPLE[0][0]), frobnicate_me=True),
                          detail="COMPATIBILITY §7.6: dropped silently, not rejected"))
    cases.append(AuthHeaderCase("errors", "auth via x-api-key (not Authorization)",
                                "/chat/completions", chat_body(CHAT_SAMPLE[0][0]),
                                detail="COMPATIBILITY §7.3: six header names authenticate"))
    if not args.skip_oversize:
        pad = "x" * (args.oversize_mb * 1024 * 1024)
        cases.append(OversizeCase("errors", "oversized body (%d MiB)" % args.oversize_mb,
                              "/chat/completions",
                              {"model": "no-such-model-oversize",
                               "messages": [{"role": "user", "content": pad}],
                               "max_tokens": 1},
                              detail="413 / request_too_large. The model name is "
                                     "deliberately nonexistent so that a gateway which "
                                     "does NOT cap the body still cannot dispatch it",
                              timeout=600))
    return cases


class OversizeCase(JSONCase):
    """Upload a body past the gateway's cap.

    It cannot go through urllib. A gateway that answers 413 mid-upload closes
    the connection before the client has finished writing, and stdlib urllib
    treats that as a transport failure and never reads the response that is
    already on the socket -- so urllib reports ECONNRESET where curl, urllib3
    and httpx all report a clean 413. That is a property of the *client*, and a
    harness that reported it as a gateway difference would be lying. So this
    case drives http.client directly and reads the response even when the write
    raised, which is what a client library that handles early responses does.
    """

    def run(self, gw):
        import http.client
        import urllib.parse
        u = urllib.parse.urlsplit(gw.base)
        cls = http.client.HTTPSConnection if u.scheme == "https" else http.client.HTTPConnection
        conn = cls(u.netloc, timeout=self.timeout)
        payload = json.dumps(self.body).encode()
        t0 = time.time()
        early = None
        target = urllib.parse.urlsplit(gw.resolve(self.path)).path
        try:
            conn.putrequest("POST", target)
            conn.putheader("Authorization", "Bearer " + gw.key)
            conn.putheader("Content-Type", "application/json")
            conn.putheader("Content-Length", str(len(payload)))
            conn.endheaders()
            try:
                conn.send(payload)
            except (BrokenPipeError, ConnectionResetError, OSError) as e:
                early = "write aborted by the gateway (%s)" % type(e).__name__
            r = conn.getresponse()
            a = Answer(r.status, dict(r.getheaders()), r.read(), time.time() - t0)
            if early:
                a.headers["x-parity-note"] = early
            return a
        except Exception as e:
            return Answer(None, {}, b"", time.time() - t0,
                          transport_error="%s: %s" % (type(e).__name__, e))
        finally:
            conn.close()

    def extras(self, ref, cand):
        out = []
        for label, a in (("litellm", ref), ("dorang", cand)):
            note = a.headers.get("x-parity-note")
            if note:
                out.append({"kind": "upload", "path": "early-close",
                            "ref": "client finished writing",
                            "cand": "%s: %s" % (label, note), "sev": "med"})
        return out


class AuthHeaderCase(JSONCase):
    """Same request, credential presented as `x-api-key` rather than Bearer."""

    def run(self, gw):
        return gw.call(self.path, self.body, headers={"x-api-key": gw.key},
                       key=None, timeout=self.timeout)


class EmbedCase(JSONCase):
    def extras(self, ref, cand):
        out = []
        rd = (ref.json() or {}).get("data") or []
        cd = (cand.json() or {}).get("data") or []
        if ref.status == 200 and cand.status == 200:
            if len(rd) != len(cd):
                out.append({"kind": "invariant", "path": "data.length",
                            "ref": len(rd), "cand": len(cd), "sev": "high"})
            rdim = [len(x.get("embedding") or []) for x in rd]
            cdim = [len(x.get("embedding") or []) for x in cd]
            if rdim != cdim:
                out.append({"kind": "invariant", "path": "embedding.dimensions",
                            "ref": rdim, "cand": cdim, "sev": "high"})
            ridx = [x.get("index") for x in rd]
            cidx = [x.get("index") for x in cd]
            if ridx != cidx:
                out.append({"kind": "invariant", "path": "data[].index ordering",
                            "ref": ridx, "cand": cidx, "sev": "high"})
        return out


class RerankCase(JSONCase):
    def extras(self, ref, cand):
        out = []
        if ref.status != 200 or cand.status != 200:
            return out
        rj, cj = ref.json() or {}, cand.json() or {}
        rr = rj.get("results") or []
        cr = cj.get("results") or []
        if len(rr) != len(cr):
            out.append({"kind": "invariant", "path": "results.length",
                        "ref": len(rr), "cand": len(cr), "sev": "high"})
        # Rank order is a property of the model, not the gateway, but the top
        # document should agree for a query this unambiguous.
        if rr and cr and rr[0].get("index") != cr[0].get("index"):
            out.append({"kind": "invariant", "path": "results[0].index",
                        "ref": rr[0].get("index"), "cand": cr[0].get("index"),
                        "sev": "med"})
        for label, j in (("litellm", rj), ("dorang", cj)):
            for r in (j.get("results") or []):
                if "relevance_score" not in r:
                    out.append({"kind": "invariant", "path": "results[].relevance_score",
                                "ref": "present", "cand": "%s: absent" % label,
                                "sev": "high"})
                    break
        return out


class MessagesCase(JSONCase):
    def extras(self, ref, cand):
        out = []
        for label, a in (("litellm", ref), ("dorang", cand)):
            j = a.json() or {}
            if a.status == 200:
                if j.get("type") != "message":
                    out.append({"kind": "invariant", "path": "type",
                                "ref": "message", "cand": "%s: %r" % (label, j.get("type")),
                                "sev": "high"})
            else:
                # COMPATIBILITY §11.1: the outer "type":"error" is load-bearing.
                if j.get("type") != "error":
                    out.append({"kind": "invariant", "path": "(error) type",
                                "ref": "error",
                                "cand": "%s: %r" % (label, j.get("type")), "sev": "high"})
        return out


class TranscribeCase(JSONCase):
    """Multipart upload. The clip is produced once by the reference gateway's
    own TTS route, so the harness carries no binary fixture."""

    CLIP = None
    CLIP_NAME = "clip.mp3"
    CLIP_TYPE = "audio/mpeg"

    @staticmethod
    def synth_wav(seconds=0.25, rate=8000):
        """A valid silent WAV, built here rather than committed as a fixture.

        The TTS route is what normally mints the clip, and on this deployment it
        answers 500 -- so without this the transcription case would never run
        and the multipart path would go untested. Silence transcribes to an
        empty string, which is fine: what is being compared is the route, the
        multipart plumbing and the response shape, not what the model heard.
        """
        import io
        import wave
        buf = io.BytesIO()
        with wave.open(buf, "wb") as w:
            w.setnchannels(1)
            w.setsampwidth(2)
            w.setframerate(rate)
            w.writeframes(b"\x00\x00" * int(rate * seconds))
        return buf.getvalue()

    def run(self, gw):
        if TranscribeCase.CLIP is None:
            TranscribeCase.CLIP = TranscribeCase.synth_wav()
            TranscribeCase.CLIP_NAME = "clip.wav"
            TranscribeCase.CLIP_TYPE = "audio/wav"
        boundary = "----parity" + uuid.uuid4().hex
        parts = []
        for field, value in (("model", TRANSCRIBE_MODEL), ("response_format", "json")):
            parts.append(("--%s\r\nContent-Disposition: form-data; name=\"%s\"\r\n\r\n%s\r\n"
                          % (boundary, field, value)).encode())
        parts.append(("--%s\r\nContent-Disposition: form-data; name=\"file\"; "
                      "filename=\"%s\"\r\nContent-Type: %s\r\n\r\n"
                      % (boundary, TranscribeCase.CLIP_NAME,
                         TranscribeCase.CLIP_TYPE)).encode())
        parts.append(TranscribeCase.CLIP)
        parts.append(("\r\n--%s--\r\n" % boundary).encode())
        return gw.call(self.path, b"".join(parts),
                       content_type="multipart/form-data; boundary=" + boundary,
                       timeout=self.timeout)


# --------------------------------------------------------------------------
# Runner
# --------------------------------------------------------------------------

VERDICT_ORDER = {"FAIL": 0, "DIFF": 1, "BOTH-ERR": 2, "SKIP": 3, "PASS": 4}


def gateway_headers(a):
    """The gateway-authored response headers. Tooling reads these (COMPATIBILITY
    §7.7), so a swap that drops them is a client-visible change even when every
    body matches. `x-litellm-model-api-base` additionally names the real
    upstream, which is the only place outside `GET /model/info` that does."""
    return {k: v for k, v in a.headers.items()
            if k.startswith("x-litellm-") or k.startswith("x-dorang-")
            or k in ("retry-after", "x-request-id")}


def verdict_for(ref, cand, diffs):
    if ref.transport_error and cand.transport_error:
        return "BOTH-ERR"
    if cand.transport_error:
        return "FAIL"
    if ref.status != cand.status:
        return "DIFF"
    highs = [d for d in diffs if d["sev"] == "high"]
    if highs:
        return "DIFF"
    if diffs:
        return "DIFF"
    if ref.status and ref.status >= 400:
        return "BOTH-ERR" if ref.status == cand.status else "DIFF"
    return "PASS"


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--only", action="append", default=[],
                    help="run only these groups (repeatable)")
    ap.add_argument("--skip", action="append", default=[], help="skip these groups")
    ap.add_argument("--list", action="store_true", help="list cases and exit, spending nothing")
    ap.add_argument("--json", metavar="FILE", help="write full results as JSON")
    ap.add_argument("--markdown", metavar="FILE",
                    help="write the parity table as a markdown table, so REPORT.md's "
                         "table is generated from data rather than transcribed")
    ap.add_argument("--chat-limit", type=int, default=len(CHAT_SAMPLE))
    ap.add_argument("--embed-limit", type=int, default=len(EMBED_SAMPLE))
    ap.add_argument("--oversize-mb", type=int, default=33,
                    help="body size for the oversize case; 33 clears dorang's 32 MiB cap")
    ap.add_argument("--skip-oversize", action="store_true")
    ap.add_argument("--include-image", action="store_true",
                    help="actually generate an image (COSTS MONEY)")
    ap.add_argument("--include-transcribe", action="store_true", default=True)
    ap.add_argument("--no-transcribe", dest="include_transcribe", action="store_false")
    ap.add_argument("--messages-model", default="gpt-oss:20b")
    ap.add_argument("--verbose", "-v", action="store_true")
    args = ap.parse_args()

    lit_base = os.environ.get("LITELLM_OPENAI_BASE_URL")
    lit_key = os.environ.get("LITELLM_OPENAI_API_KEY")
    dor_base = os.environ.get("DORANG_BASE_URL", "http://127.0.0.1:4199")
    dor_key = os.environ.get("DORANG_API_KEY") or os.environ.get("DORANG_MASTER_KEY")
    missing = [n for n, v in (("LITELLM_OPENAI_BASE_URL", lit_base),
                              ("LITELLM_OPENAI_API_KEY", lit_key),
                              ("DORANG_API_KEY", dor_key)) if not v]
    if missing and not args.list:
        sys.exit("missing environment: %s\n(source ~/env, then export DORANG_API_KEY)"
                 % ", ".join(missing))

    cases = build_cases(args)
    if args.only:
        cases = [c for c in cases if c.group in args.only]
    if args.skip:
        cases = [c for c in cases if c.group not in args.skip]

    if args.list:
        for c in cases:
            print("%-14s %s" % (c.group, c.name))
        print("\n%d cases, %d HTTP calls (one per gateway)" % (len(cases), 2 * len(cases)))
        return 0

    ref_gw = Gateway("litellm", lit_base, lit_key)
    cand_gw = Gateway("dorang", dor_base, dor_key)

    results = []
    for c in cases:
        # The transcription clip is minted once, by the reference gateway's own
        # TTS route below, and the same bytes then go to both.
        ref = c.run(ref_gw)
        cand = c.run(cand_gw)
        if isinstance(c, BinaryCase) and ref.status == 200 and ref.body:
            TranscribeCase.CLIP = ref.body
        diffs = c.compare(ref, cand) + c.extras(ref, cand)
        v = verdict_for(ref, cand, diffs)
        results.append({
            "group": c.group, "name": c.name, "detail": c.detail, "verdict": v,
            "litellm": {"status": ref.status, "ctype": ref.ctype,
                        "bytes": len(ref.body), "ms": int(ref.elapsed * 1000),
                        "transport_error": ref.transport_error,
                        "gateway_headers": gateway_headers(ref)},
            "dorang": {"status": cand.status, "ctype": cand.ctype,
                       "bytes": len(cand.body), "ms": int(cand.elapsed * 1000),
                       "transport_error": cand.transport_error,
                       "gateway_headers": gateway_headers(cand)},
            "diffs": diffs,
            "litellm_body": ref.body[:1200].decode("utf-8", "replace"),
            "dorang_body": cand.body[:1200].decode("utf-8", "replace"),
        })
        print("  %-9s %-58s litellm=%-4s dorang=%-4s %s"
              % (v, c.name[:58], ref.status, cand.status,
                 "%d diff" % len(diffs) if diffs else ""), flush=True)

    # ---- table ----
    print("\n" + "=" * 100)
    print("PARITY TABLE  (reference = LiteLLM, candidate = dorang)")
    print("=" * 100)
    print("%-13s %-52s %-8s %-8s %-9s %s"
          % ("GROUP", "CASE", "LITELLM", "DORANG", "VERDICT", "DIFFS"))
    print("-" * 100)
    for r in results:
        print("%-13s %-52s %-8s %-8s %-9s %d"
              % (r["group"], r["name"][:52], r["litellm"]["status"],
                 r["dorang"]["status"], r["verdict"], len(r["diffs"])))

    counts = {}
    for r in results:
        counts[r["verdict"]] = counts.get(r["verdict"], 0) + 1
    print("-" * 100)
    print("  ".join("%s=%d" % (k, counts[k])
                    for k in sorted(counts, key=lambda x: VERDICT_ORDER.get(x, 9))))

    print("\n" + "=" * 100)
    print("DIVERGENCES, high severity first")
    print("=" * 100)
    for sev in ("high", "med", "low"):
        rows = [(r, d) for r in results for d in r["diffs"] if d["sev"] == sev]
        if not rows:
            continue
        print("\n--- %s (%d) ---" % (sev.upper(), len(rows)))
        for r, d in rows:
            print("  [%s] %s\n      %s %s\n      litellm: %r\n      dorang : %r"
                  % (r["group"], r["name"], d["kind"], d["path"], d["ref"], d["cand"]))

    if args.markdown:
        with open(args.markdown, "w") as f:
            f.write("| Group | Case | LiteLLM | dorang | Verdict | Diffs |\n")
            f.write("|---|---|---:|---:|---|---:|\n")
            for r in results:
                f.write("| %s | `%s` | %s | %s | %s | %d |\n" % (
                    r["group"], r["name"],
                    r["litellm"]["status"] or "(transport)",
                    r["dorang"]["status"] or "(transport)",
                    r["verdict"], len(r["diffs"])))
        print("\nwrote %s" % args.markdown)

    if args.json:
        with open(args.json, "w") as f:
            json.dump({"generated": time.strftime("%Y-%m-%dT%H:%M:%S"),
                       "results": results, "counts": counts}, f, indent=1)
        print("\nwrote %s" % args.json)

    return 1 if any(r["verdict"] == "FAIL" for r in results) else 0


if __name__ == "__main__":
    sys.exit(main())
