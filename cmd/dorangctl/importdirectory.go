package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/ziozzang/dorang/internal/app"
	"github.com/ziozzang/dorang/internal/store"
)

// importDirectory implements `dorangctl import teams` and `import users`.
//
// # Why two more verbs
//
// `import keys` carries a key's team_id and user_id and then warns that the
// team or user is not present, and MIGRATION §1.2 told the operator that a
// plain import "cannot have planted them". The rows a key refers to carry the
// budget ceiling, the rate limit, the model allow-list and the blocked flag
// that the request path now enforces — so a fleet of imported keys without
// their teams runs with every team-scoped limit silently absent, which is the
// fail-open failure the whole of §3 is written against. These verbs create
// the rows, from the incumbent's own tables, with the same report-first,
// write-on-`--commit` discipline as the key import.
//
// Order matters and is stated rather than inferred: users first, then teams
// with `--members`, then keys. A team's membership names users, and a member
// whose user row is absent is reported and not carried.
func (e env) importDirectory(verb string, args []string) int {
	fs := newFlagSet("import "+verb, e)
	defTable := store.DefaultTeamTable
	if verb == "users" {
		defTable = store.DefaultUserTable
	}
	var (
		cfgPath          = configPathFlag(fs)
		from             = fs.String("from", "", "source database: a postgres:// URL or a SQLite file path")
		driver           = fs.String("from-driver", "", "postgres or sqlite; inferred from --from when unset")
		table            = fs.String("table", defTable, "source table")
		onUntranslatable = fs.String("on-untranslatable", string(store.UntranslatableSkip),
			"skip: refuse a row whose model allow-list holds an idiom dorang cannot express; "+
				"clear: import it with that allow-list dropped, named per row in the report")
		limit  = fs.Int("limit", 0, "read at most N source rows (0: all)")
		commit = fs.Bool("commit", false,
			"write the imported rows. Without it this reads and reports, and changes nothing")
		members = fs.Bool("members", true,
			"teams: carry members_with_roles into team_members. Import users first; a member "+
				"whose user is absent is reported, not invented")
		emailDomain = fs.String("synthetic-email-domain", "",
			"users: give a user without an email `<user_id>@<domain>`; dorang requires one. "+
				"Unset skips such users, reported")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*from) == "" {
		fmt.Fprintf(e.stderr, "usage: dorangctl import %s --from <dsn> [--commit]\n", verb)
		return 2
	}
	untranslatable := store.UntranslatablePolicy(*onUntranslatable)
	if untranslatable != store.UntranslatableSkip && untranslatable != store.UntranslatableClear {
		return e.fail("--on-untranslatable: want %q or %q, got %q",
			store.UntranslatableSkip, store.UntranslatableClear, *onUntranslatable)
	}
	d, err := sourceDriver(*driver, *from)
	if err != nil {
		return e.fail("%v", err)
	}
	cfg, ok := e.loadConfigFile(*cfgPath)
	if !ok {
		return 1
	}
	ctx := context.Background()
	dst, err := app.OpenStore(ctx, cfg, false)
	if err != nil {
		return e.fail("%v", err)
	}
	defer dst.Close()
	src, err := store.OpenSource(ctx, d, *from)
	if err != nil {
		return e.fail("%v", err)
	}
	defer src.Close()

	opts := store.DirectoryImportOptions{
		Table:                *table,
		OnUntranslatable:     untranslatable,
		DryRun:               !*commit,
		Limit:                *limit,
		Members:              *members,
		SyntheticEmailDomain: strings.TrimSpace(*emailDomain),
	}
	var rep store.DirectoryReport
	switch verb {
	case "teams":
		rep, err = dst.ImportTeams(ctx, src, opts)
	default:
		rep, err = dst.ImportUsers(ctx, src, opts)
	}
	if err != nil {
		return e.fail("%v", err)
	}
	fmt.Fprint(e.stdout, rep.Summary())
	if !*commit {
		fmt.Fprintln(e.stdout,
			"nothing was written. Re-run with --commit once the report above is what you expect")
	}
	return 0
}
