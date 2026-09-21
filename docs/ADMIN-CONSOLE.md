# Administrative console

Open `/ui/`. Sign in with the master credential or an administrator API key. The header language selector supports 한국어 and English; the initial language follows the browser, and a selection persists in `localStorage` as `dorang.locale`. Theme uses a separate `dorang.theme` preference. Credentials and telemetry are not stored there.

## Screens and data sources

| Category | Screen | Source / controls |
| --- | --- | --- |
| Observe | Overview | Node request rates, errors, latency, per-model requests, token kinds, priced and unpriced usage |
| Observe | Live requests | Bounded metadata buffer; search, status filter, pause and request details |
| Observe | Request history | Persisted `/spend/logs`; bounded dates, key/user/team/trace/tag filters and cursor pagination |
| Observe | Prometheus | Registry exposition; search all families and labels, categories, rates and raw export |
| Route & serve | Models & deployments | Configured routes, aliases and deployment enable/disable controls |
| Route & serve | Credentials / Capacity | Quota and OAuth admission status, occupancy and reservation axes |
| Access | Keys / Users & teams | Existing key lifecycle, access limits, user/team membership controls |
| Cost & usage | Usage report | Fleet ledger rollups by model, key, team or day |
| Cost & usage | Budgets | Create/edit/remove key, user and team ceilings through the administrative API |
| Cost & usage | Price preview | Configured rule evaluation, token dimensions, components and unpriced results |
| System | Model catalog / Operations | Capability provenance, unverified entries, subsystem dependencies, reload and API route inventory |

The route inventory distinguishes registered handlers from unsupported stubs. A registered handler can still require a subsystem that is not configured; it does not imply a dedicated editor for every API operation. Unsupported schema concepts are not offered as working controls. Team-scoped operators retain their permitted access screens; global telemetry and fleet reports require global scope.

## Real-time behavior and accounting

`/ui/events` uses authenticated SSE. It sends the same registry and reporters exposed by the API every two seconds, sharing a two-second snapshot cache between viewers. Streams reconnect after 25 seconds, revalidate authorization before every frame, and stop while the tab is hidden or paused. Each process allows 16 simultaneous streams and uses a five-second write deadline. Proxy buffering must be disabled for this route (`X-Accel-Buffering: no` is emitted).

Live requests contain the latest 256 completed inference requests within 15 minutes **on the connected node**. Request bodies, response bodies, authorization headers and provider error messages are not captured. The existing error type is exposed as bounded metadata. This view is not a Docker stdout tail or a durable audit log. Historical queries use the persistent ledger; all-status history requires an indexed key, user, team, trace or tag filter. Multiple filters are applied together within bounded cursor pages, so a page can be sparse.

Prometheus counters are per process, since restart. Overview HTTP rates and in-flight values include management traffic; model rows identify inference traffic. Load balancer node changes reset the in-browser rate history. Scrape each node externally for durable fleet-wide Prometheus history. The database-backed usage report is fleet-wide.

Cache read/write tokens are subsets of input, and reasoning tokens are a subset of output. They are shown separately and not added again to totals. `dorang_model_tokens_total`, `dorang_model_cost_nano_total`, and `dorang_model_pricing_requests_total` use the existing admitted-model/cardinality limits. Unpriced requests are displayed explicitly instead of being treated as free usage.

Credential latency/failure counters use the same bounded recent buffer, not lifetime counters. Unknown admission health is shown as unknown. Deployment circuit statistics remain in their Prometheus families and are not attributed to every credential sharing a deployment. No database migration is required for this console update; the user history query uses the existing user/time index.

## Localization

`ui/assets/i18n.js` owns the English-to-Korean catalog, language selection, static text/attribute translation, parameterized notices and the `dorang:language` event. Live views call `window.dorangI18n.t()` and rerender on that event. Add English UI text and a Korean catalog entry together. Mark opaque identifiers with `data-no-i18n`; API paths, metric names, labels, source help and raw diagnostic responses retain their original spelling. Without JavaScript, server-rendered forms remain available in English; SSE views link to a server-rendered snapshot.

## Verification

```sh
go test ./...
go test -race ./internal/admin ./internal/metrics ./internal/app -run 'TrafficBuffer|Telemetry|ModelTokenAndCost|ObserveAndGatherRaceFree'
```

The opt-in browser fixture uses the real application, SQLite, session auth, administrative handlers and metrics registry. It seeds observer events without making paid upstream requests, and exits after ten minutes:

```sh
DORANG_CONSOLE_FIXTURE=/tmp/dorang-console-fixture.json \
  go test ./internal/app -run '^TestConsoleBrowserFixture$' -count=1 -timeout 11m
# In another terminal with Playwright installed:
node ui/tests/console.mjs
# If Playwright is installed elsewhere, set PLAYWRIGHT_MODULE to its module path.
```

The browser smoke verifies language selection/persistence, localized title and live status, SSE samples, all token kinds, pause/filter/details, metric inspection, navigation, pricing, user creation, budget creation/update/removal, dark theme and mobile overflow. Screenshots are written under `/tmp/dorang-console-screenshots/` and fixture secrets are disposable test credentials.

## Compose rollout on this host

The production project is `/docker/build/dorang`, building from `DORANG_SOURCE=./src`. The active application services are `dorang` and `dorang-2`; both use `dorang:local`, a shared configuration bind and PostgreSQL, with separate state volumes. `dorang-3` is a scale-out test definition and is not part of the normal rollout.

Sync the verified source into `src`, preserving deployment `.env`, configuration and volumes. Keep the previous image under a rollback tag. Build `dorang` once, then replace `dorang-2` and verify it is healthy before replacing `dorang`. Use `docker compose up -d --no-deps --no-build <service>`; do not run `down` or recreate the database. Verify both the health command and the UI assets/SSE after deployment. The 60-second Compose grace period exceeds the configured drain window; the 25-second SSE lifetime allows streams to finish within the application shutdown grace. Direct port 4000 belongs only to the first node; the existing HA proxy is a separate path.

## Provider → account → model workflow

`/ui/setup` is the starting point for new upstream connections. It edits the configuration actually compiled by the router; it does not use the disconnected model database registry. The three steps are provider protocol/endpoint, upstream account authentication, then service-model bindings. Saving a new connection carries its selection into account registration; saving an account carries its selection into model setup. The existing Models screen controls activation. New bindings default to disabled.

`GET /admin/setup` returns secret-free desired settings, a configuration revision and the current node's application state. `POST /admin/setup/change` requires that revision, global administrative authority and an audit sink. Concurrent stale edits return 409. It preserves unrelated YAML fields/comments, validates the candidate configuration and adapter, and uses the existing cross-process configuration file lock. UI forms also require the session CSRF token.

API keys and static access tokens can be entered or referenced by file/environment variable. Existing values are never returned. Replacement writes a new mode-0600 file under `secrets/` beside the configuration, then switches the configuration reference and reloads; it preserves the account ID and every model binding. Superseded protected files are retained for rollback and are not automatically deleted. They should be included in protected operational backups, not source control.

OAuth token stores can be supplied as JSON or referenced by file/environment variable in the supported formats. Existing advanced refresh settings are preserved on source updates. This is token-store configuration, not a new vendor browser authorization flow. Adding/changing OAuth account configuration remains subject to the gateway's existing restart requirement: the UI reports `restart_required` and displays desired versus active state. It does not claim a hot reload applied those accounts. New environment variables likewise require container recreation. Check each node after a rolling restart.

`POST /admin/setup/discover` explicitly checks the running account against a supported upstream model-list endpoint. It sends no inference request and does not follow redirects. Requests have a 10-second timeout and 2-MiB response bound; at most 1,000 model IDs from the returned page are displayed. Raw upstream error bodies are never echoed. Unsupported discovery surfaces have a manual-entry path; listing a model does not establish that a later inference request will succeed. Pricing and quotas still follow the gateway's configured policies.

The Compose deployment must mount the same protected directory at `/etc/dorang/secrets` on both nodes. The directory must be writable by UID 65532, with mode 0700. A node's private state volume is unsuitable for these references because another node could not load the new credentials. No Docker socket is exposed to the application; a required restart is an operational rollout, not an arbitrary process-control API.
