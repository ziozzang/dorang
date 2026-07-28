package capacity

// Axis identifies one dimension a concurrency limit can be counted over.
// The order of the constants is the order axes are checked and committed in,
// and it matches DESIGN §5.7: the two non-provider axes (global, principal)
// come first, then the provider-scoped ones.
type Axis uint8

const (
	// AxisGlobal is the process-wide ceiling. It has no key.
	AxisGlobal Axis = iota
	// AxisPrincipal is per caller (api key, user or team id).
	AxisPrincipal
	// AxisRoute is a single provider deployment's own ceiling. Key: provider.
	AxisRoute
	// AxisProviderGroup is a pool shared by several providers. Key: group name.
	AxisProviderGroup
	// AxisModel is per (provider, upstream model).
	AxisModel
	// AxisCredentialGroup is per account, across all models. Key: group name.
	AxisCredentialGroup
	// AxisKey is a single credential's own ceiling. Key: (provider, credential id).
	AxisKey

	numAxes = int(AxisKey) + 1
)

// String returns the short axis name used in configuration and metrics.
func (a Axis) String() string {
	switch a {
	case AxisGlobal:
		return "global"
	case AxisPrincipal:
		return "principal"
	case AxisRoute:
		return "route"
	case AxisProviderGroup:
		return "pgroup"
	case AxisModel:
		return "model"
	case AxisCredentialGroup:
		return "cgroup"
	case AxisKey:
		return "key"
	}
	return "unknown"
}

// axisKey identifies one counted bucket.
//
// The two compound axes (model, key) keep their components in separate fields
// rather than in one concatenated string. Concatenating would allocate on every
// acquisition, which DESIGN §15.2 rules out for the hot path; a comparable
// struct is just as usable as a map key and allocates nothing.
type axisKey struct {
	axis Axis
	a, b string
}

// String renders the key for humans and metrics. It never contains credential
// material: the key axis is keyed by credential *id*, not by the secret.
func (k axisKey) String() string {
	if k.b != "" {
		return k.axis.String() + ":" + k.a + "|" + k.b
	}
	return k.axis.String() + ":" + k.a
}

func globalKey() axisKey                          { return axisKey{axis: AxisGlobal} }
func principalKey(id string) axisKey              { return axisKey{axis: AxisPrincipal, a: id} }
func routeKey(provider string) axisKey            { return axisKey{axis: AxisRoute, a: provider} }
func pgroupKey(group string) axisKey              { return axisKey{axis: AxisProviderGroup, a: group} }
func modelAxisKey(provider, model string) axisKey { return axisKey{AxisModel, provider, model} }
func cgroupKey(group string) axisKey              { return axisKey{axis: AxisCredentialGroup, a: group} }
func credAxisKey(provider, id string) axisKey     { return axisKey{AxisKey, provider, id} }

// modelIdent is the configuration lookup key for a per-model limit. Like
// axisKey it is a comparable struct so lookups do not allocate.
type modelIdent struct {
	provider string
	model    string
}

// axisNeed is one axis a candidate requires, together with the configured limit
// for it. Limits travel with the request because one of them (the credential
// key ceiling) is supplied by the caller rather than by Config.
type axisNeed struct {
	key   axisKey
	limit int
	// queue is the configured `max_queue` for this key, 0 for unbounded. It
	// rides along with the concurrency limit because the two are looked up from
	// the same name at the same moment, and a second pass over the axes to
	// fetch it would double the map lookups on the hot path.
	queue int
}
