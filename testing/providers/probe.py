#!/usr/bin/env python3
"""Bring every real provider up on dorang and record what came back.

ONE CALL PER CASE. Every chat case asks for at most 24 output tokens from a
one-sentence prompt; nothing here loops, sweeps or retries. `--list` prints the
case table and spends nothing.

The interesting cases are the four-way ones. z.ai and Qwen each expose the same
account behind an OpenAI-shaped base URL and an Anthropic-shaped one, and dorang
converts between client protocol and upstream protocol. So each of those two
vendors is probed as a 2x2:

                       client /v1/chat/completions   client /v1/messages
    OpenAI upstream            relay                      convert
    Anthropic upstream         convert                    relay

If §10.7's equivalence claim holds, a client sees the same SHAPE in all four
cells — same field names, same finish reason vocabulary, same usage accounting —
whatever the upstream spoke.

Environment: DORANG_BASE_URL, DORANG_API_KEY.
"""

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request

DORANG = os.environ.get("DORANG_BASE_URL", "").rstrip("/")
KEY = os.environ.get("DORANG_API_KEY", "")

PROMPT = "Reply with the single word: ok"
MAX_TOKENS = 24

results = []


# ---------------------------------------------------------------------------
# transport
# ---------------------------------------------------------------------------

def call(path, body=None, method=None, stream=False, timeout=180):
    url = DORANG + path
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("Authorization", "Bearer " + KEY)
    req.add_header("X-Dorang-Detail", "full")
    if data is not None:
        req.add_header("Content-Type", "application/json")
    t0 = time.time()
    out = {"status": 0, "elapsed": 0.0, "headers": {}, "body": None,
           "text": None, "error": None}
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            out["status"] = r.status
            out["headers"] = {k.lower(): v for k, v in r.headers.items()
                              if k.lower().startswith("x-dorang")}
            raw = r.read()
            out["text"] = raw.decode("utf-8", "replace")
            if not stream:
                try:
                    out["body"] = json.loads(raw or b"{}")
                except ValueError:
                    pass
    except urllib.error.HTTPError as e:
        out["status"] = e.code
        out["headers"] = {k.lower(): v for k, v in e.headers.items()
                          if k.lower().startswith("x-dorang")}
        raw = e.read()
        out["text"] = raw.decode("utf-8", "replace")
        try:
            out["error"] = json.loads(raw or b"{}")
        except ValueError:
            out["error"] = {"raw": out["text"][:400]}
    except Exception as e:  # noqa: BLE001
        out["error"] = {"transport": repr(e)}
    out["elapsed"] = round(time.time() - t0, 2)
    return out


# ---------------------------------------------------------------------------
# request shapes
# ---------------------------------------------------------------------------

def oai_chat(model, stream=False, usage=False):
    b = {"model": model, "max_tokens": MAX_TOKENS,
         "messages": [{"role": "user", "content": PROMPT}]}
    if stream:
        b["stream"] = True
        if usage:
            # OpenAI emits stream usage only on this opt-in, so asking for it is
            # the only way to tell "dorang dropped the counts" from "dorang
            # honoured the protocol".
            b["stream_options"] = {"include_usage": True}
    return call("/v1/chat/completions", b, stream=stream)


def ant_messages(model, stream=False):
    b = {"model": model, "max_tokens": MAX_TOKENS,
         "messages": [{"role": "user", "content": PROMPT}]}
    if stream:
        b["stream"] = True
    return call("/v1/messages", b, stream=stream)


def ant_count(model):
    return call("/v1/messages/count_tokens",
                {"model": model, "messages": [{"role": "user", "content": PROMPT}]})


def embeddings(model):
    return call("/v1/embeddings", {"model": model, "input": "hello"})


def rerank(model):
    return call("/v1/rerank", {
        "model": model,
        "query": "what is the capital of France",
        "documents": ["Paris is the capital of France.", "Bananas are yellow."],
        "top_n": 2})


# ---------------------------------------------------------------------------
# what a result MEANS, in the two client vocabularies
# ---------------------------------------------------------------------------

def summarize(r, kind):
    """The shape a client actually sees, reduced to something comparable."""
    if r["status"] != 200:
        err = r.get("error") or {}
        e = err.get("error") if isinstance(err, dict) else None
        msg = (e or err or {}).get("message") if isinstance(e or err, dict) else None
        return {"ok": False, "status": r["status"],
                "type": (e or {}).get("type") if isinstance(e, dict) else None,
                "code": (e or {}).get("code") if isinstance(e, dict) else None,
                "message": (msg or r.get("text", ""))[:200]}
    b = r["body"]
    if kind == "sse":
        events, models, finals = [], set(), []
        for line in (r["text"] or "").splitlines():
            if line.startswith("event: "):
                events.append(line[7:].strip())
            elif line.startswith("data: "):
                payload = line[6:].strip()
                if payload == "[DONE]":
                    events.append("[DONE]")
                    continue
                try:
                    obj = json.loads(payload)
                except ValueError:
                    continue
                if isinstance(obj, dict):
                    if obj.get("model"):
                        models.add(obj["model"])
                    if obj.get("type"):
                        events.append(obj["type"])
                    for ch in obj.get("choices") or []:
                        if ch.get("finish_reason"):
                            finals.append(ch["finish_reason"])
        seen, order = set(), []
        for e in events:
            if e not in seen:
                seen.add(e)
                order.append(e)
        return {"ok": True, "status": 200, "frames": len(events),
                "event_kinds": order[:12], "model": sorted(models)[:1],
                "finish": finals[:2]}
    if kind == "oai":
        ch = (b.get("choices") or [{}])[0]
        u = b.get("usage") or {}
        return {"ok": True, "status": 200, "object": b.get("object"),
                "model": b.get("model"),
                "id_prefix": (b.get("id") or "")[:9],
                "role": (ch.get("message") or {}).get("role"),
                "content": ((ch.get("message") or {}).get("content") or "")[:40],
                "finish_reason": ch.get("finish_reason"),
                "usage_keys": sorted(u.keys()),
                "prompt_tokens": u.get("prompt_tokens"),
                "completion_tokens": u.get("completion_tokens")}
    if kind == "ant":
        u = b.get("usage") or {}
        content = b.get("content") or []
        text = "".join(c.get("text", "") for c in content if isinstance(c, dict))
        return {"ok": True, "status": 200, "type": b.get("type"),
                "model": b.get("model"),
                "id_prefix": (b.get("id") or "")[:9],
                "role": b.get("role"),
                "content_types": [c.get("type") for c in content if isinstance(c, dict)],
                "content": text[:40],
                "stop_reason": b.get("stop_reason"),
                "usage_keys": sorted(u.keys()),
                "input_tokens": u.get("input_tokens"),
                "output_tokens": u.get("output_tokens")}
    if kind == "embed":
        d = (b.get("data") or [{}])[0]
        vec = d.get("embedding") or []
        return {"ok": True, "status": 200, "object": b.get("object"),
                "model": b.get("model"), "n": len(b.get("data") or []),
                "dims": len(vec) if isinstance(vec, list) else "not-a-list",
                # the whole object: which counts a vendor reports, and which of
                # them dorang can meter, is the question this case answers
                "usage": b.get("usage"),
                "usage_keys": sorted((b.get("usage") or {}).keys())}
    if kind == "rerank":
        res = b.get("results") or []
        return {"ok": True, "status": 200, "model": b.get("model"),
                "n": len(res),
                "keys": sorted(res[0].keys()) if res else [],
                "top_index": res[0].get("index") if res else None,
                "has_score": bool(res and ("relevance_score" in res[0] or "score" in res[0])),
                "usage": b.get("usage"),
                "usage_keys": sorted((b.get("usage") or {}).keys())}
    if kind == "count":
        return {"ok": True, "status": 200, "keys": sorted(b.keys()),
                "input_tokens": b.get("input_tokens")}
    if kind == "models":
        ids = [m.get("id") for m in (b.get("data") or [])]
        return {"ok": True, "status": 200, "n": len(ids), "ids": ids}
    return {"ok": True, "status": 200, "raw": str(b)[:200]}


# ---------------------------------------------------------------------------
# the case table
# ---------------------------------------------------------------------------
# (case id, provider, what it exercises, callable, summary kind)
CASES = [
    ("catalog.models", "dorang", "GET /v1/models",
     lambda: call("/v1/models", method="GET"), "models"),

    # --- Ollama: two accounts on one provider ------------------------------
    ("ollama.acct1.chat", "ollama", "chat, credential ollama-1 only",
     lambda: oai_chat("ollama-acct1"), "oai"),
    ("ollama.acct2.chat", "ollama", "chat, credential ollama-2 only",
     lambda: oai_chat("ollama-acct2"), "oai"),
    ("ollama.pool.chat", "ollama", "chat, both credentials eligible",
     lambda: oai_chat("ollama-pool"), "oai"),

    # --- z.ai 2x2: one account, two upstream protocols --------------------
    ("zai.oaiup.oaiclient", "zai", "openai upstream <- openai client (relay)",
     lambda: oai_chat("zai-oai-glm46"), "oai"),
    ("zai.oaiup.antclient", "zai", "openai upstream <- anthropic client (convert)",
     lambda: ant_messages("zai-oai-glm46"), "ant"),
    ("zai.antup.oaiclient", "zai", "anthropic upstream <- openai client (convert)",
     lambda: oai_chat("zai-ant-glm46"), "oai"),
    ("zai.antup.antclient", "zai", "anthropic upstream <- anthropic client (relay)",
     lambda: ant_messages("zai-ant-glm46"), "ant"),
    ("zai.oaiup.oaiclient.stream", "zai", "openai upstream -> openai SSE (relay reference)",
     lambda: oai_chat("zai-oai-glm46", stream=True), "sse"),
    ("zai.antup.oaiclient.stream", "zai", "anthropic upstream -> openai SSE",
     lambda: oai_chat("zai-ant-glm46", stream=True), "sse"),
    ("zai.oaiup.antclient.stream", "zai", "openai upstream -> anthropic SSE",
     lambda: ant_messages("zai-oai-glm46", stream=True), "sse"),
    ("zai.antup.oaiclient.stream.usage", "zai",
     "anthropic upstream -> openai SSE with stream_options.include_usage",
     lambda: oai_chat("zai-ant-glm46", stream=True, usage=True), "sse"),
    ("zai.antup.count_tokens", "zai", "POST /v1/messages/count_tokens",
     lambda: ant_count("zai-ant-glm46"), "count"),

    # --- the GLM coding plan as a Claude-shaped client addresses it --------
    ("glmplan.antclient", "glm-plan", "anthropic upstream <- anthropic client",
     lambda: ant_messages("glm-plan"), "ant"),

    # --- Qwen 2x2 ----------------------------------------------------------
    ("qwen.oaiup.oaiclient", "qwen", "openai upstream <- openai client (relay)",
     lambda: oai_chat("qwen-oai-flash"), "oai"),
    ("qwen.oaiup.antclient", "qwen", "openai upstream <- anthropic client (convert)",
     lambda: ant_messages("qwen-oai-flash"), "ant"),
    ("qwen.antup.oaiclient", "qwen", "anthropic upstream <- openai client (convert)",
     lambda: oai_chat("qwen-ant-flash"), "oai"),
    ("qwen.antup.antclient", "qwen", "anthropic upstream <- anthropic client (relay)",
     lambda: ant_messages("qwen-ant-flash"), "ant"),

    # --- the rest ----------------------------------------------------------
    ("openrouter.chat", "openrouter", "chat on a zero-price model",
     lambda: oai_chat("openrouter-free"), "oai"),
    ("hf.chat", "huggingface", "chat through the HF router",
     lambda: oai_chat("hf-qwen3-8b"), "oai"),

    ("jina.embeddings", "jina", "embeddings, dedicated adapter, direct",
     lambda: embeddings("jina-embed-nano"), "embed"),
    ("jina.rerank", "jina", "rerank, dedicated adapter, direct",
     lambda: rerank("jina-rerank"), "rerank"),

    ("litellm.chat", "litellm", "chat through the operator's LiteLLM",
     lambda: oai_chat("litellm-chat"), "oai"),
    ("litellm.jina.embeddings", "litellm", "the same Jina model via LiteLLM",
     lambda: embeddings("litellm-jina-embed-nano"), "embed"),
    ("litellm.jina.rerank", "litellm", "the same Jina reranker via LiteLLM",
     lambda: rerank("litellm-jina-rerank"), "rerank"),
]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--list", action="store_true", help="print the case table, spend nothing")
    ap.add_argument("--only", action="append", default=[],
                    help="run only cases whose id contains this substring")
    ap.add_argument("--json", help="write the full result set here")
    args = ap.parse_args()

    if args.list:
        for cid, prov, what, _, _ in CASES:
            print("%-30s %-12s %s" % (cid, prov, what))
        print("\n%d cases; every chat case asks for at most %d output tokens."
              % (len(CASES), MAX_TOKENS))
        return 0

    if not DORANG or not KEY:
        print("DORANG_BASE_URL and DORANG_API_KEY must be set", file=sys.stderr)
        return 2

    selected = [c for c in CASES
                if not args.only or any(o in c[0] for o in args.only)]
    print("running %d of %d cases against %s\n" % (len(selected), len(CASES), DORANG))

    for cid, prov, what, fn, kind in selected:
        r = fn()
        s = summarize(r, kind)
        s["elapsed"] = r["elapsed"]
        # every X-Dorang-* header, not a chosen subset: the ABSENCE of one is
        # evidence too, and a filter would hide which ones never appeared
        s["dorang"] = {k[9:]: v for k, v in r["headers"].items()
                       if k not in ("x-dorang-request-id",)}
        results.append({"case": cid, "provider": prov, "what": what,
                        "kind": kind, "result": s,
                        # the bytes a client actually received, so the report
                        # can quote a shape rather than paraphrase one
                        "raw": (r.get("text") or "")[:8000]})
        mark = "ok  " if s.get("ok") else "FAIL"
        print("%s %-30s %-12s %s" % (mark, cid, prov, what))
        print("     %s" % json.dumps(s, sort_keys=True)[:600])

    print("\n---- by provider ----")
    byprov = {}
    for row in results:
        byprov.setdefault(row["provider"], []).append(row["result"].get("ok"))
    for p in sorted(byprov):
        v = byprov[p]
        print("  %-12s %d/%d" % (p, sum(1 for x in v if x), len(v)))

    if args.json:
        with open(args.json, "w") as f:
            json.dump(results, f, indent=2, sort_keys=True)
        print("\nwrote %s" % args.json)
    return 0


if __name__ == "__main__":
    sys.exit(main())
