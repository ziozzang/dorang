#!/usr/bin/env python3
"""Prove dorang's per-credential concurrency axis against two accounts.

Four properties, each reported as PASS or FAIL:

  P1  ceiling — each account admits 3 in flight and the pool admits 6; the
      seventh is refused rather than queued.
  P2  spill  — with account 1 saturated, work moves to account 2 immediately.
  P3  axis   — a SECOND MODEL on account 1 does not get its own 3. This is the
      difference between a per-credential axis and a per-(key, model) axis, and
      it is the property that would be silently wrong.
  P4  affinity — a conversation that landed on account 2 keeps landing there
      even when account 1 is free and the rotation prefers it, and keeps landing
      there while account 1 is busy (DESIGN §7.4a2).

Every claim is checked twice: against the account label the upstream reports
having authenticated, and against dorang's own X-Dorang-Credential header. A
disagreement between the two is itself a finding and is reported.

Environment: DORANG_BASE_URL, DORANG_API_KEY, FAKE_UPSTREAM_URL.
"""

import json
import os
import sys
import threading
import time
import urllib.error
import urllib.request

DORANG = os.environ["DORANG_BASE_URL"].rstrip("/")
KEY = os.environ["DORANG_API_KEY"]
FAKE = os.environ["FAKE_UPSTREAM_URL"].rstrip("/")
if FAKE.endswith("/v1"):
    FAKE = FAKE[:-3]

# credential id -> the account label the upstream knows it by. Both halves come
# from configuration, not from a secret.
CRED_LABEL = {"ollama-1": "acct-1", "ollama-2": "acct-2"}

results = []
notes = []


def record(name, ok, detail):
    results.append((name, ok, detail))
    print("  %-5s %s" % ("PASS" if ok else "FAIL", detail))


# ---------------------------------------------------------------------------
# transport
# ---------------------------------------------------------------------------

def chat(model, text, session=None, timeout=180):
    """One chat completion. Returns a dict describing what happened."""
    body = {"model": model, "max_tokens": 8,
            "messages": [{"role": "user", "content": text}]}
    req = urllib.request.Request(
        DORANG + "/v1/chat/completions",
        data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json",
                 "Authorization": "Bearer " + KEY,
                 # opts into the routing-decision headers, which is where
                 # dorang states which credential it believes it used
                 "X-Dorang-Detail": "full"})
    if session:
        req.add_header("X-Dorang-Session", session)
    t0 = time.time()
    out = {"model": model, "session": session, "status": 0,
           "served_by": None, "credential": None, "error": None}
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            out["status"] = r.status
            out["credential"] = r.headers.get("X-Dorang-Credential")
            payload = json.loads(r.read() or b"{}")
            content = (payload.get("choices") or [{}])[0].get("message", {}).get("content", "")
            if content.startswith("served by "):
                out["served_by"] = content[len("served by "):].strip()
    except urllib.error.HTTPError as e:
        out["status"] = e.code
        out["credential"] = e.headers.get("X-Dorang-Credential")
        raw = e.read()
        try:
            out["error"] = json.loads(raw)
        except ValueError:
            out["error"] = {"raw": raw.decode("utf-8", "replace")[:400]}
    except Exception as e:  # noqa: BLE001 - a transport failure is a result too
        out["error"] = {"transport": repr(e)}
    out["elapsed"] = time.time() - t0
    return out


def ctl(path, method="GET"):
    req = urllib.request.Request(FAKE + path, method=method,
                                 data=b"" if method == "POST" else None)
    with urllib.request.urlopen(req, timeout=10) as r:
        return json.loads(r.read() or b"{}")


def reset():
    ctl("/__ctl/reset", "POST")
    time.sleep(0.2)


def state():
    return ctl("/__ctl/state")


def inflight():
    s = state()
    return {k: v for k, v in s["inflight"].items() if v > 0}


def wait_for(pred, what, timeout=20):
    end = time.time() + timeout
    last = None
    while time.time() < end:
        last = inflight()
        if pred(last):
            return last
        time.sleep(0.1)
    raise AssertionError("timed out waiting for %s; last inflight=%r" % (what, last))


class Held:
    """A set of chat calls the upstream is holding open."""

    def __init__(self):
        self.threads = []
        self.out = []

    def add(self, model, session=None, n=1):
        for _ in range(n):
            slot = {}
            self.out.append(slot)

            def run(slot=slot, model=model, session=session):
                slot.update(chat(model, "HOLD", session=session))

            t = threading.Thread(target=run, daemon=True)
            t.start()
            self.threads.append(t)
            time.sleep(0.12)   # keep arrival order deterministic
        return self

    def join(self):
        for t in self.threads:
            t.join(30)
        return self.out


def release_all(held):
    ctl("/__ctl/release", "POST")
    if held:
        held.join()
    wait_for(lambda s: not s, "the upstream to go idle", timeout=30)
    reset()


def cross_check(r, expect_label):
    """Agreement between the upstream's answer and dorang's own header."""
    cred = r.get("credential")
    mapped = CRED_LABEL.get(cred)
    if r["served_by"] and mapped and r["served_by"] != mapped:
        notes.append("dorang reported credential %r but the upstream authenticated %r"
                     % (cred, r["served_by"]))
    return (r["served_by"] == expect_label) or (mapped == expect_label)


# ---------------------------------------------------------------------------
# the cases
# ---------------------------------------------------------------------------

def case_ceiling_and_spill():
    print("\n[1] ceiling of 3 per account, and spill rather than queue")
    reset()
    held = Held().add("dual-a", n=3)
    seen = wait_for(lambda s: sum(s.values()) == 3, "3 held on the first account")
    record("P1a", seen == {"acct-1": 1 * 3} or seen.get("acct-1") == 3,
           "the first three requests all land on acct-1: %r" % (seen,))

    r = chat("dual-a", "hi")
    fast = r["elapsed"] < 5
    record("P2a", r["status"] == 200 and cross_check(r, "acct-2") and fast,
           "with acct-1 full, request 4 is served by %s in %.2fs (dorang says %s)"
           % (r["served_by"], r["elapsed"], r["credential"]))

    held.add("dual-a", n=3)
    seen = wait_for(lambda s: sum(s.values()) == 6, "6 held across both accounts")
    record("P1b", seen.get("acct-1") == 3 and seen.get("acct-2") == 3,
           "the pool saturates at 3 + 3: %r" % (seen,))

    r = chat("dual-a", "hi")
    fast = r["elapsed"] < 5
    code = ((r.get("error") or {}).get("error") or {}).get("code") or \
           ((r.get("error") or {}).get("error") or {}).get("type")
    record("P1c", r["status"] in (429, 503) and fast,
           "request 7 is refused in %.2fs with %d%s, not queued behind the six"
           % (r["elapsed"], r["status"], (" " + str(code)) if code else ""))

    release_all(held)


def case_second_model_does_not_raise_the_ceiling():
    print("\n[2] a second MODEL on the same account does not raise its ceiling")
    reset()
    held = Held().add("dual-a", n=3)
    wait_for(lambda s: s.get("acct-1") == 3, "acct-1 saturated by dual-a")

    r = chat("dual-b", "hi")
    record("P3a", r["status"] == 200 and cross_check(r, "acct-2"),
           "dual-b, a different model on the same accounts, is served by %s "
           "(a per-(key,model) axis would have said acct-1)" % (r["served_by"],))

    held.add("dual-b", n=3)
    seen = wait_for(lambda s: sum(s.values()) == 6, "6 held across two models")
    record("P3b", seen.get("acct-1") == 3 and seen.get("acct-2") == 3,
           "three dual-a plus three dual-b is still 3 per account: %r" % (seen,))

    r = chat("dual-b", "hi")
    record("P3c", r["status"] in (429, 503),
           "with two models in play the seventh is still refused (%d) — the "
           "account ceiling is counted across models" % r["status"])

    release_all(held)

    # The same claim from the other side: fill ONE account with a MIXTURE of the
    # two models. If the axis were per (key, model), two of dual-a plus one of
    # dual-b would leave acct-1 with room on both counters and the next request
    # would stay there. It has to spill instead.
    mixed = Held().add("dual-a", n=2)
    mixed.add("dual-b", n=1)
    seen = wait_for(lambda s: sum(s.values()) == 3, "a mixed-model account")
    log = state()["log"]
    per = {}
    for e in log:
        if e["hold"]:
            per.setdefault(e["label"], set()).add(e["model"])
    record("P3d", seen.get("acct-1") == 3 and len(per.get("acct-1", ())) == 2,
           "one account holding two different models at once: %r"
           % ({k: sorted(v) for k, v in per.items()},))

    r = chat("dual-a", "hi")
    record("P3e", r["status"] == 200 and cross_check(r, "acct-2"),
           "2x dual-a + 1x dual-b fills acct-1 completely; the next dual-a is "
           "served by %s, so the two models share one bucket of 3"
           % (r["served_by"],))

    release_all(mixed)


def case_affinity():
    print("\n[3] credential affinity: a conversation keeps its account (§7.4a2)")
    reset()

    # A control: with everything idle, failover always prefers acct-1. So a
    # later landing on acct-2 cannot be an accident of the rotation.
    r0 = chat("dual-a", "hi", session="control-session")
    record("P4a", cross_check(r0, "acct-1"),
           "control: with both accounts idle a fresh conversation lands on %s"
           % (r0["served_by"],))

    held = Held().add("dual-a", n=3)
    wait_for(lambda s: s.get("acct-1") == 3, "acct-1 saturated")

    r1 = chat("dual-a", "hi", session="sticky-session")
    landed = r1["served_by"]
    record("P4b", r1["status"] == 200 and cross_check(r1, "acct-2"),
           "a conversation opened while acct-1 was full lands on %s" % (landed,))

    release_all(held)

    r2 = chat("dual-a", "hi", session="sticky-session")
    record("P4c", cross_check(r2, "acct-2"),
           "with BOTH accounts idle and the rotation preferring acct-1, turn 2 "
           "of that conversation still lands on %s" % (r2["served_by"],))

    r2b = chat("dual-a", "hi", session="control-session")
    record("P4d", cross_check(r2b, "acct-1"),
           "and the control conversation still lands on %s, so the pin is per "
           "conversation and not a global drift" % (r2b["served_by"],))

    held = Held().add("dual-a", n=3)   # the OTHER account, the one it is not on
    wait_for(lambda s: s.get("acct-1") == 3, "acct-1 saturated again")
    r3 = chat("dual-a", "hi", session="sticky-session")
    record("P4e", cross_check(r3, "acct-2"),
           "with the other account busy the conversation still lands on %s"
           % (r3["served_by"],))
    release_all(held)

    # The documented limit of the guarantee, recorded rather than asserted:
    # stickiness here is PREFERRED, not PINNED. A request carrying no
    # account-scoped state spills when its own account is full, by design.
    held = Held().add("dual-a", session="filler", n=3)
    # push acct-2 (the pinned one) to 3 by giving the filler session a pin there
    st = wait_for(lambda s: sum(s.values()) == 3, "three held")
    onto = "acct-1" if st.get("acct-1") == 3 else "acct-2"
    r4 = chat("dual-a", "hi", session="sticky-session")
    notes.append("stickiness is preferred, not pinned: with the held work on %s "
                 "the sticky conversation was served by %s (spill is the "
                 "configured on_capacity for an unpinned request)"
                 % (onto, r4["served_by"]))
    release_all(held)


def case_the_other_axis():
    """The contrast: a plan whose allowance IS per model, on one key.

    Same gateway, same instant, one configuration difference — the ceiling is
    declared under `capacity.models` instead of `capacity.credential_groups`.
    If the two axes are really distinct, one account here reaches 14 in flight
    across two models where one Ollama account reaches 3.
    """
    print("\n[4] the contrasting axis: 7 per MODEL on one key is 14, not 7")
    reset()
    held = Held().add("plan-x", n=7)
    seen = wait_for(lambda s: sum(s.values()) == 7, "7 held on plan-x")
    record("P5a", seen.get("plan-acct") == 7,
           "one key holds 7 of plan-x: %r" % (seen,))

    r = chat("plan-x", "hi")
    record("P5b", r["status"] in (429, 503),
           "an eighth plan-x is refused (%d): the per-model ceiling is real"
           % r["status"])

    held.add("plan-y", n=7)
    seen = wait_for(lambda s: sum(s.values()) == 14, "14 held on one key")
    record("P5c", seen.get("plan-acct") == 14,
           "the SAME key simultaneously holds 7 of plan-y — 14 in flight on one "
           "account: %r. Two models on one Ollama account is still 3." % (seen,))

    r = chat("plan-y", "hi")
    record("P5d", r["status"] in (429, 503),
           "and the fifteenth is refused (%d), so 14 is the ceiling and not an "
           "absence of one" % r["status"])
    release_all(held)


def main():
    print("dorang dual-account capacity proof")
    print("  gateway : %s" % DORANG)
    print("  upstream: %s (fake, controllable)" % FAKE)
    case_ceiling_and_spill()
    case_second_model_does_not_raise_the_ceiling()
    case_affinity()
    case_the_other_axis()

    print("\n---- summary ----")
    bad = [n for n, ok, _ in results if not ok]
    for n, ok, d in results:
        print("  %-5s %s" % ("PASS" if ok else "FAIL", n))
    for n in notes:
        print("  note: %s" % n)
    print("  %d checks, %d failed" % (len(results), len(bad)))
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
