package luaext

import lua "github.com/yuin/gopher-lua"

// The Lua spelling of the views.
//
// A hook sees a table whose keys are exactly the identifiers a policy program
// can name — same names, same values, same absences. That is not a coincidence
// worth preserving for tidiness: it means the sandbox argument in view.go
// applies unchanged to Lua. There is no key for a header map, a body, or a
// credential, so a plugin cannot read one, and with no I/O it could not send one
// anywhere if it could.
//
// The table is built fresh for each handler rather than reused. Reuse would save
// an allocation on a path that is off by default, and would let one plugin
// change what the next one is told.

func requestTable(L *lua.LState, v *RequestView) lua.LValue {
	t := L.NewTable()
	t.RawSetString("request_id", lua.LString(v.RequestID))
	t.RawSetString("method", lua.LString(v.Method))
	t.RawSetString("path", lua.LString(v.Path))
	t.RawSetString("route", lua.LString(v.Route))
	t.RawSetString("model", lua.LString(v.Model))
	t.RawSetString("key_id", lua.LString(v.KeyID))
	t.RawSetString("key_name", lua.LString(v.KeyName))
	t.RawSetString("user_id", lua.LString(v.UserID))
	t.RawSetString("team_id", lua.LString(v.TeamID))
	t.RawSetString("priority", lua.LString(v.Priority))
	t.RawSetString("body_bytes", lua.LNumber(v.BodyBytes))
	t.RawSetString("input_tokens", lua.LNumber(v.InputTokens))
	t.RawSetString("max_output_tokens", lua.LNumber(v.MaxOutputTokens))
	t.RawSetString("stream", lua.LBool(v.Stream))
	return t
}

func routeTable(L *lua.LState, v *RouteView) lua.LValue {
	t := L.NewTable()
	t.RawSetString("model", lua.LString(v.Model))
	t.RawSetString("key_id", lua.LString(v.KeyID))
	t.RawSetString("user_id", lua.LString(v.UserID))
	t.RawSetString("team_id", lua.LString(v.TeamID))
	t.RawSetString("provider", lua.LString(v.Provider))
	t.RawSetString("deployment", lua.LString(v.Deployment))
	t.RawSetString("kind", lua.LString(v.Kind))
	t.RawSetString("upstream_model", lua.LString(v.UpstreamModel))
	t.RawSetString("priority", lua.LString(v.Priority))
	t.RawSetString("attempt", lua.LNumber(v.Attempt))
	t.RawSetString("input_tokens", lua.LNumber(v.InputTokens))
	t.RawSetString("stream", lua.LBool(v.Stream))
	return t
}

func responseTable(L *lua.LState, v *ResponseView) lua.LValue {
	t := L.NewTable()
	t.RawSetString("request_id", lua.LString(v.RequestID))
	t.RawSetString("model", lua.LString(v.Model))
	t.RawSetString("key_id", lua.LString(v.KeyID))
	t.RawSetString("user_id", lua.LString(v.UserID))
	t.RawSetString("team_id", lua.LString(v.TeamID))
	t.RawSetString("provider", lua.LString(v.Provider))
	t.RawSetString("deployment", lua.LString(v.Deployment))
	t.RawSetString("upstream_model", lua.LString(v.UpstreamModel))
	t.RawSetString("error_code", lua.LString(v.ErrorCode))
	t.RawSetString("status", lua.LNumber(v.Status))
	t.RawSetString("input_tokens", lua.LNumber(v.InputTokens))
	t.RawSetString("output_tokens", lua.LNumber(v.OutputTokens))
	t.RawSetString("cost_nano_usd", lua.LNumber(v.CostNanoUSD))
	t.RawSetString("ttft_ms", lua.LNumber(v.TTFTMillis))
	t.RawSetString("total_ms", lua.LNumber(v.TotalMillis))
	t.RawSetString("attempts", lua.LNumber(v.Attempts))
	t.RawSetString("stream", lua.LBool(v.Stream))
	return t
}

func emailTable(L *lua.LState, v *EmailView) lua.LValue {
	t := L.NewTable()
	t.RawSetString("event", lua.LString(v.Event))
	t.RawSetString("subject_kind", lua.LString(v.SubjectKind))
	t.RawSetString("subject_id", lua.LString(v.SubjectID))
	t.RawSetString("recipient", lua.LString(v.Recipient))
	t.RawSetString("subject", lua.LString(v.Subject))
	t.RawSetString("body", lua.LString(v.Body))
	t.RawSetString("driver", lua.LString(v.Driver))
	return t
}
