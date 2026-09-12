package admin

// register wires the whole administrative surface.
//
// It is one function on purpose. §2.3 fixes a list of paths and §0.2 fixes what
// happens to everything else, so the single most useful thing this package can
// offer a reviewer is one place where the whole surface — implemented, stubbed
// and refused — can be read against those two sections in a minute.
func (a *API) register() {
	a.registerShapeCompatible()
	a.registerNative()
	a.registerStubs()
}

// registerShapeCompatible mounts the paths of DESIGN §2.3, whose shape exists so
// that scripts and UIs written against the incumbent keep working.
func (a *API) registerShapeCompatible() {
	// Keys.
	a.write("/key/generate", (*call).keyGenerate)
	a.read("/key/info", (*call).keyInfo)
	a.write("/key/update", (*call).keyUpdate)
	a.write("/key/delete", (*call).keyDelete)
	a.read("/key/list", (*call).keyList)
	a.write("/key/block", keySetBlocked(true))
	a.write("/key/unblock", keySetBlocked(false))
	a.write("/key/regenerate", (*call).keyRegenerate)

	// Rotation (§11.2c) and the pend/release pair (§11.6). They are additive:
	// no existing reader breaks on a route it does not call.
	a.write("/key/rotate", (*call).keyRotate)
	a.write("/key/rotate/cut", (*call).keyCutGrace)
	a.read("/key/secrets", (*call).keySecrets)
	a.write("/key/pend", keySetPended(true))
	a.write("/key/release", keySetPended(false))

	// Users.
	a.write("/user/new", (*call).userNew)
	a.read("/user/info", (*call).userInfo)
	a.write("/user/update", (*call).userUpdate)
	a.write("/user/delete", (*call).userDelete)
	a.read("/user/list", (*call).userList)

	// Teams.
	a.write("/team/new", (*call).teamNew)
	a.read("/team/info", (*call).teamInfo)
	a.write("/team/update", (*call).teamUpdate)
	a.write("/team/delete", (*call).teamDelete)
	a.read("/team/list", (*call).teamList)
	a.write("/team/member_add", (*call).teamMemberAdd)
	a.write("/team/member_delete", (*call).teamMemberDelete)

	// Models and model groups.
	a.write("/model/new", (*call).modelNew)
	a.read("/model/info", (*call).modelInfo)
	a.write("/model/update", (*call).modelUpdate)
	a.write("/model/delete", (*call).modelDelete)
	// Take a deployment in or out of routing by editing the config file. Unlike
	// the CRUD above (which writes a DB registry nothing routes on, so it 501s),
	// this edits the config the router actually compiles — see [ConfigWriter].
	a.write("/model/deployment/set_enabled", (*call).modelDeploymentSetEnabled)
	a.read("/model_group/info", (*call).modelGroupInfo)

	// Budgets. new and update share one implementation, differing in whether
	// the budget must already exist.
	a.write("/budget/new", budgetSet(false))
	a.read("/budget/info", (*call).budgetInfo)
	a.write("/budget/update", budgetSet(true))
	a.write("/budget/delete", (*call).budgetDelete)
	a.read("/budget/list", (*call).budgetList)

	// Spend.
	a.read("/spend/logs", (*call).spendLogs)
	a.write("/spend/calculate", (*call).spendCalculate)
	a.read("/global/spend/report", (*call).globalSpendReport)

	// Daily activity. One implementation, three dimensions: the queries differ
	// only in which column they group by, and three copies of this would drift
	// in exactly the way the notional and cost columns must not.
	a.read("/user/daily/activity", dailyActivity(GroupByUser, "user_id", "user_ids"))
	a.read("/team/daily/activity", dailyActivity(GroupByTeam, "team_id", "team_ids"))
	a.read("/tag/daily/activity", dailyActivity(GroupByTag, "tag", "tags"))

	// Health.
	a.read("/health/history", (*call).healthHistory)
}

// registerNative mounts the /admin/* surface: what dorang has and the incumbent
// does not, namespaced so a future shape-compatible path can never collide.
func (a *API) registerNative() {
	a.read("/admin/status", (*call).adminStatus)
	a.read("/admin/credentials/health", (*call).adminCredentials)
	a.read("/admin/quota", (*call).adminQuota)
	a.read("/admin/capacity", (*call).adminCapacity)
	a.read("/admin/catalog/explain", (*call).adminCatalogExplain)
	a.read("/admin/catalog/unverified", (*call).adminCatalogUnverified)
	a.write("/admin/pricing/preview", (*call).adminPricingPreview)
	a.write("/admin/config/reload", (*call).adminConfigReload)
}

// registerStubs names the administrative surfaces dorang deliberately does not
// implement, so that each answers 501 with a code that says *which* thing is
// missing rather than the generic route-level refusal.
//
// The distinction is worth the lines. A caller hitting an unregistered path
// learns "not an implemented route"; a caller hitting one of these learns that
// the concept does not exist in dorang's domain model and what to use instead.
// §0.2 requires the 501; being specific about it is what makes the 501 useful.
func (a *API) registerStubs() {
	for _, s := range []struct{ path, code, reason string }{
		{"/organization/new", "organizations_unsupported", organizationsReason},
		{"/organization/info", "organizations_unsupported", organizationsReason},
		{"/organization/update", "organizations_unsupported", organizationsReason},
		{"/organization/delete", "organizations_unsupported", organizationsReason},
		{"/organization/list", "organizations_unsupported", organizationsReason},
		{"/organization/member_add", "organizations_unsupported", organizationsReason},
		{"/organization/member_delete", "organizations_unsupported", organizationsReason},

		{"/customer/new", "customers_unsupported", customersReason},
		{"/customer/info", "customers_unsupported", customersReason},
		{"/customer/update", "customers_unsupported", customersReason},
		{"/customer/delete", "customers_unsupported", customersReason},
		{"/customer/list", "customers_unsupported", customersReason},

		{"/key/health", "key_health_unsupported",
			"a key has no health of its own in dorang; health belongs to credentials and " +
				"deployments (DESIGN §7.5a). Use /admin/credentials/health"},

		{"/global/spend/reset", "spend_reset_unsupported",
			"the ledger is append-only and spend is derived from it, so there is nothing to " +
				"reset. Adjust the budget with /budget/update, or let the period roll over"},

		{"/spend/tags", "spend_tags_unsupported",
			"tag reporting is served by /tag/daily/activity and by /global/spend/report with " +
				"group_by=tag, both of which take the bounded range DESIGN §9.3 requires"},

		{"/model/settings", "model_settings_unsupported",
			"deployment parameters live on the deployment; read them from /model/info and " +
				"write them with /model/update"},

		{"/cache/ping", "cache_admin_unsupported", cacheReason},
		{"/cache/flushall", "cache_admin_unsupported", cacheReason},
		{"/cache/delete", "cache_admin_unsupported", cacheReason},

		{"/budget/settings", "budget_settings_unsupported",
			"dorang has no reusable named-budget object: a budget is a ceiling on a subject " +
				"(DESIGN §6.4, §9.2). Use /budget/new with subject_kind and subject_id"},

		{"/audit/list", "audit_read_unsupported",
			"audit rows are written by every mutation but are not yet readable over the API; " +
				"query the audit_logs table directly until this ships"},
	} {
		a.stub(s.path, s.code, s.reason)
	}
}

const organizationsReason = "dorang's domain model has users and teams (DESIGN §11.4); an " +
	"organization is a column on teams, not an object with its own lifecycle. Use /team/* and " +
	"set organization_id"

const customersReason = "dorang has no end-customer object: spend attribution is by key, user, " +
	"team and tag (DESIGN §9.3). Tag requests and report with /tag/daily/activity"

const cacheReason = "dorang's caches are the prefix chain and the router's affinity state " +
	"(DESIGN §7.4), neither of which is a key-value store an operator flushes; there is nothing " +
	"here to administer"
