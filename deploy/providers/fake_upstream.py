#!/usr/bin/env python3
"""A controllable OpenAI-shaped upstream, for proving dorang's capacity model.

WHY A FAKE
    The properties under test are about WHICH ACCOUNT a request lands on when
    another account is full. Demonstrating that against a real provider means
    holding several requests open on a subscription plan the user depends on,
    and the real provider cannot tell us which credential served a request
    anyway.  This upstream can: it reports the account label for every call it
    answered, and it can hold a call open until told to let go, which is what
    saturates a credential without spending anything.

ACCOUNTS
    Read from $FAKE_UPSTREAM_ACCOUNTS, a comma-separated list of
    `label=ENV_VAR_NAME` pairs.  The VALUE is read from the environment at
    start-up and never written anywhere; only the label appears in the log,
    in this file, or in anything this file prints.  The keys themselves are
    generated per run by the launcher and are not credentials.

PROTOCOL
    POST /v1/chat/completions   the OpenAI chat surface, enough of it.
        A request whose last user message begins with "HOLD" blocks until
        released, so the caller controls exactly how long a slot stays taken.
    POST /v1/embeddings         a stub, for the multi-class case.

CONTROL PLANE (not part of the OpenAI surface; no auth, loopback only)
    GET  /__ctl/state    {"inflight": {label: n}, "log": [...], "held": n}
    POST /__ctl/release  let every held call finish
    POST /__ctl/reset    drop the log and release everything
"""

import json
import os
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

# label -> secret, built from $FAKE_UPSTREAM_ACCOUNTS at start-up.
ACCOUNTS = {}
BY_SECRET = {}

_lock = threading.Lock()
_release = threading.Event()
_log = []          # [{"label":..,"model":..,"start":..,"end":..,"held":bool}]
_inflight = {}     # label -> count
_held = 0
_seq = 0


def _load_accounts():
    spec = os.environ.get("FAKE_UPSTREAM_ACCOUNTS", "")
    for item in spec.split(","):
        item = item.strip()
        if not item:
            continue
        label, _, var = item.partition("=")
        secret = os.environ.get(var)
        if not secret:
            sys.stderr.write("fake_upstream: %s is not set in the environment\n" % var)
            raise SystemExit(2)
        ACCOUNTS[label] = secret
        BY_SECRET[secret] = label
    if not ACCOUNTS:
        sys.stderr.write("fake_upstream: FAKE_UPSTREAM_ACCOUNTS names no accounts\n")
        raise SystemExit(2)


def _label_for(auth_header):
    if not auth_header:
        return None
    token = auth_header.strip()  # pragma: allowlist secret — parses an inbound header
    if token.lower().startswith("bearer "):
        token = token[7:].strip()
    return BY_SECRET.get(token)


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_args):
        pass  # the control plane is the log

    # -- helpers ----------------------------------------------------------
    def _json(self, status, obj):
        body = json.dumps(obj).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _read_body(self):
        n = int(self.headers.get("Content-Length") or 0)
        return self.rfile.read(n) if n else b""

    # -- control plane ----------------------------------------------------
    def do_GET(self):
        if self.path == "/__ctl/state":
            with _lock:
                self._json(200, {
                    "inflight": dict(_inflight),
                    "held": _held,
                    "log": list(_log),
                })
            return
        self._json(404, {"error": {"message": "no such route"}})

    def do_POST(self):
        if self.path == "/__ctl/release":
            self._read_body()
            _release.set()
            self._json(200, {"released": True})
            return
        if self.path == "/__ctl/reset":
            self._read_body()
            _release.set()
            time.sleep(0.15)
            with _lock:
                del _log[:]
                _inflight.clear()
            _release.clear()
            self._json(200, {"reset": True})
            return
        if self.path.endswith("/chat/completions"):
            self._chat()
            return
        if self.path.endswith("/embeddings"):
            self._embeddings()
            return
        self._json(404, {"error": {"message": "no such route: " + self.path}})

    # -- the OpenAI surface -----------------------------------------------
    def _authorize(self):
        label = _label_for(self.headers.get("Authorization"))
        if label is None:
            self._json(401, {"error": {
                "message": "no account owns this credential",
                "type": "invalid_request_error"}})
            return None
        return label

    def _chat(self):
        global _held, _seq
        label = self._authorize()
        if label is None:
            self._read_body()
            return
        raw = self._read_body()
        try:
            req = json.loads(raw or b"{}")
        except ValueError:
            self._json(400, {"error": {"message": "bad json"}})
            return

        model = req.get("model", "")
        text = ""
        for m in req.get("messages", []):
            c = m.get("content")
            if isinstance(c, str):
                text = c
            elif isinstance(c, list):
                text = "".join(p.get("text", "") for p in c if isinstance(p, dict))
        hold = text.startswith("HOLD")

        start = time.time()
        with _lock:
            _seq += 1
            seq = _seq
            _inflight[label] = _inflight.get(label, 0) + 1
            if hold:
                _held += 1
            entry = {"seq": seq, "label": label, "model": model,
                     "start": start, "end": None, "hold": hold,
                     "session": self.headers.get("X-Dorang-Session") or ""}
            _log.append(entry)

        if hold:
            # 90s is a backstop against a harness that forgets to release; it is
            # far longer than any case here and shorter than dorang's timeout.
            _release.wait(90)

        with _lock:
            _inflight[label] = _inflight.get(label, 1) - 1
            if hold:
                _held -= 1
            entry["end"] = time.time()

        self._json(200, {
            "id": "chatcmpl-fake-%d" % seq,
            "object": "chat.completion",
            "created": int(start),
            "model": model,
            "choices": [{
                "index": 0,
                "message": {"role": "assistant",
                            "content": "served by %s" % label},
                "finish_reason": "stop",
            }],
            "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
        })

    def _embeddings(self):
        label = self._authorize()
        if label is None:
            self._read_body()
            return
        raw = self._read_body()
        try:
            req = json.loads(raw or b"{}")
        except ValueError:
            self._json(400, {"error": {"message": "bad json"}})
            return
        inputs = req.get("input")
        if isinstance(inputs, str):
            inputs = [inputs]
        self._json(200, {
            "object": "list",
            "model": req.get("model", ""),
            "data": [{"object": "embedding", "index": i, "embedding": [0.0, 1.0]}
                     for i in range(len(inputs or [""]))],
            "usage": {"prompt_tokens": 1, "total_tokens": 1},
        })


def main():
    _load_accounts()
    port = int(os.environ.get("FAKE_UPSTREAM_PORT", "4290"))
    srv = ThreadingHTTPServer(("127.0.0.1", port), Handler)
    srv.daemon_threads = True
    sys.stderr.write("fake_upstream: listening on 127.0.0.1:%d for %d accounts\n"
                     % (port, len(ACCOUNTS)))
    sys.stderr.flush()
    srv.serve_forever()


if __name__ == "__main__":
    main()
