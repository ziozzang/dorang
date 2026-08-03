// Package catalog carries dorang's knowledge of provider kinds and models:
// which wire adapter a kind speaks, the endpoint it addresses, its
// prompt-cache scheme, capability defaults, and — per model — the reasoning
// control the model actually accepts.
//
// # The embedded data is a default, never a ceiling
//
// Two data files ship embedded:
//
//	provider_defaults.yaml   per provider kind (DESIGN §4.3)
//	model_catalog.yaml       per model (DESIGN §10.2)
//
// Everything about them is overridable, and an operator can run dorang against
// a provider and a model this build has never heard of using configuration
// alone. Layers compose in this order, each winning over the last:
//
//  1. the embedded files,
//  2. each path given to [Load] — a file, or a directory whose *.yaml and
//     *.yml entries are applied sorted by name,
//  3. every path in [EnvCatalogPath] (DORANG_CATALOG_PATH), PATH-separated.
//
// A layer may introduce provider kinds, aliases, prefix rules and models, not
// only patch known ones. See catalog.d.example for a runnable directory.
//
// # Merge, and the one way to remove a value
//
// Entries combine field by field, so an overlay states only what it changes.
// That can never remove a value, because under field-wise merge an absent key
// and an unchanged key are the same input. An entry that must discard what it
// inherited says so:
//
//   - kind: acme
//     model: "acme-lodestar-2"
//     merge: replace          # default is `merge`
//     context_window: 190000  # and nothing else survives
//
// A document may set `merge:` once at the top to change the default for every
// entry in it. An explicit YAML null is *not* removal — it reads as "not
// stated", the same as omitting the key.
//
// No layer can delete an entry outright. A catalog that can be silently
// emptied is a catalog whose absences mean nothing.
//
// # Provenance
//
// Every resolved field records the layer and the file it came from, and
// [Catalog.Explain] reports them. An operator with a wrong context window
// needs to know which file to edit, and an operator who just wrote an overlay
// needs to know whether it applied — an overlay that missed by one byte in a
// model name looks exactly like one that applied and agreed.
//
// [Catalog.Validate] reports every problem in a loaded catalog in one pass,
// and [LintFiles] does the same for files on disk without starting anything,
// treating a malformed file as a finding rather than a panic.
//
// # Three rules this package exists to enforce
//
// Model names are opaque (REVIEW C3, DESIGN §2.1). No function here splits a
// model name on ':', '/', '[' or any other character. The only string
// operation applied to a model name is a literal prefix test, and its result
// feeds capability defaults only — never identity, routing, or hashing. A name
// is carried back out of a lookup byte-identical to the way it went in.
//
// Reasoning capability is keyed by (kind, model), never by kind alone
// (REVIEW C5). Revision 1 keyed folding on the provider kind and generalized an
// effort scale observed on one model version to a whole family, so on the
// family's other members the control would be silently ignored or rejected.
// Here, only an explicit model entry can carry a reasoning capability. Kind
// defaults carry a ReasoningHint, which names the adapter shape that exists for
// the kind and is deliberately not a capability claim about any model. Prefix
// rules cannot carry reasoning at all.
//
// Unknown is the default and is never guessed (DESIGN §10.2). An entry may
// claim a concrete reasoning capability only if it also records the date it was
// checked against a live endpoint. Loading refuses data that violates this.
// [Catalog.UnverifiedModels] reports everything still unprobed, so the gap is
// visible instead of being papered over with a plausible-looking value. The
// same discipline governs numbers: a context window that has not been observed
// is undeclared, which is not the same as zero, because a wrong window feeds
// context-window fallback routing and misroutes silently.
//
// # What the absence of a date means
//
// `verified:` records one outcome — a request naming the model was answered,
// by it. Everything else fell into "no date", and that single absence covered
// four unrelated situations that call for opposite actions. Asking the
// providers produced all four in one afternoon:
//
//   - Nobody has asked. The entry was transcribed from a third-party catalog.
//   - Somebody asked and the plan is not entitled: "Access to model denied.
//     Please make sure you are eligible for using the model." That is not a
//     retirement — a retirement names a date and a reference id — so the model
//     EXISTS, its context window is real, and another account reaches it.
//     Dating it would be false and deleting it would destroy the only written
//     record of a real model.
//   - Somebody asked and a DIFFERENT model answered. Three z.ai names return
//     200 with a body naming another model. This is the outcome that looks
//     most like success, and every number the catalog holds for the requested
//     name then describes a model the caller is not talking to.
//   - Nobody could ask: no credential for the route exists here, so no entry
//     under that kind has been checked or can be.
//
// A [Probe] records the two middle cases on an entry and the last on a kind,
// and [Catalog.Verification] collapses all of it into one answer per entry. The
// value is in what an undated entry means afterwards: with every state stated,
// "no date and no probe" narrows to "nobody has asked yet", which is a backlog
// item somebody can close rather than a shrug.
//
// A probe never becomes policy. A denial does not make a model unroutable —
// entitlement belongs to an account, and this catalog ships to operators with
// different plans. A substitution does not authorise rewriting the request to
// the served name. Both are knowledge, recorded so a human can act on them.
//
// A retirement stays a DELETION rather than a state, and the line is principled:
// a retired model serves nobody, so the entry is knowledge that expired, while
// a denied model is knowledge that is correct and merely out of reach from here.
package catalog
