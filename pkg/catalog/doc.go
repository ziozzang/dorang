// Package catalog carries dorang's knowledge of provider kinds and models:
// which wire adapter a kind speaks, its prompt-cache scheme, capability
// defaults, and — per model — the reasoning control the model actually accepts.
//
// Two data files are embedded and may be overridden or extended by operator
// files at load time:
//
//	provider_defaults.yaml   per provider kind (DESIGN §4.3)
//	model_catalog.yaml       per model (DESIGN §10.2)
//
// # Three rules this package exists to enforce
//
// Model names are opaque (REVIEW C3, DESIGN §2.1). No function here splits a
// model name on ':', '/', or any other character. The only string operation
// applied to a model name is a literal prefix test, and its result feeds
// capability defaults only — never identity, routing, or hashing. A name is
// carried back out of a lookup byte-identical to the way it went in.
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
// visible instead of being papered over with a plausible-looking value.
package catalog
