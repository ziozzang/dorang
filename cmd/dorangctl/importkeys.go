package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/ziozzang/dorang/internal/app"
	"github.com/ziozzang/dorang/internal/store"
)

// importKeys implements `dorangctl import keys`.
//
// # Why this file exists
//
// `store.ImportKeys` was implemented, tested end to end in
// testing/scenario/credential_test.go, and had **no operator-facing
// invocation**: no subcommand, no administrative endpoint. MIGRATION.md §3.5
// existed to tell an operator planning a cutover that the one thing they were
// most likely to be counting on could not be run, and to reissue instead. The
// mechanism, the expiry rule, the fail-open column list and the report were all
// there; this is the missing line.
//
// # Why it does not write by default
//
// Importing credentials into a live gateway grants access. `--commit` is
// therefore explicit and the default is the store's own DryRun, which reads the
// source, resolves every row and prints the identical report without writing.
// That is not caution for its own sake: the report is the whole point of the
// import. It names every row that did NOT migrate and why, and every source
// column that was not carried — and DESIGN §2.4 records that the majority of
// stored credentials in a real deployment are already expired, so the report is
// what tells an operator whether the import is worth committing at all.
//
// # The secret is never needed and never printed
//
// The source stores sha256 of the full token, and dorang's index key is the
// first half of exactly that digest, so nothing here has a plaintext credential
// to leak. What identifies a row in the output is its `lookup` — derived from
// the digest, reveals nothing about the secret — which is why the report can be
// printed in full.
func (e env) importKeys(args []string) int {
	fs := newFlagSet("import keys", e)
	cfgPath := configPathFlag(fs)
	var (
		from = fs.String("from", "",
			"source database DSN (required): a postgres URL, or a path to a SQLite file")
		driver = fs.String("from-driver", "",
			"source driver: postgres or sqlite (default: inferred from --from)")
		table = fs.String("table", store.DefaultImportTable,
			"source table holding the credentials")
		onMissingTeam = fs.String("on-missing-team", string(store.MissingTeamSkip),
			"skip: refuse a key whose team is absent; orphan: import it with the team dropped")
		onUntranslatable = fs.String("on-untranslatable", string(store.UntranslatableSkip),
			"skip: refuse a key whose allow-list holds an idiom dorang cannot express, such as "+
				"a route group or a model sentinel; clear: import it with that allow-list dropped, "+
				"which is a widening and is named per key in the report")
		objectPermTable = fs.String("object-permission-table", store.DefaultObjectPermissionTable,
			"source table an object_permission_id points into. It is READ: a key whose model "+
				"restriction lives there and is not resolved imports with an empty allow-list, "+
				"and empty allows every model")
		limit  = fs.Int("limit", 0, "read at most N source rows (0: all)")
		source = fs.String("source", "", `value recorded in api_keys.source (default "litellm")`)
		commit = fs.Bool("commit", false,
			"write the imported keys. Without it this reads and reports, and changes nothing")
		expectAdminKey = fs.Bool("expect-admin-key", false,
			"you believe the administrative credential is among the rows. It never is, and "+
				"the report will say so: dorang's master key is compared out-of-band and is not a row")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*from) == "" {
		fmt.Fprintln(e.stderr, "usage: dorangctl import keys --from <dsn> [--commit]")
		return 2
	}
	policy := store.MissingTeamPolicy(*onMissingTeam)
	if policy != store.MissingTeamSkip && policy != store.MissingTeamOrphan {
		return e.fail("--on-missing-team: want %q or %q, got %q",
			store.MissingTeamSkip, store.MissingTeamOrphan, *onMissingTeam)
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
	// The destination is migrated: importing into a schema that predates
	// api_key_secrets would write the denormalized copy and leave the rows
	// authentication actually reads empty, which is the shape /key/regenerate
	// failed in.
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

	rep, err := dst.ImportKeys(ctx, src, store.ImportOptions{
		Table:                 *table,
		OnMissingTeam:         policy,
		OnUntranslatable:      untranslatable,
		ObjectPermissionTable: *objectPermTable,
		ExpectAdminKey:        *expectAdminKey,
		DryRun:                !*commit,
		Limit:                 *limit,
		Source:                *source,
	})
	if err != nil {
		return e.fail("%v", err)
	}
	fmt.Fprint(e.stdout, rep.Summary())
	if !*commit {
		fmt.Fprintln(e.stdout,
			"nothing was written. Re-run with --commit once the report above is what you expect")
	}
	// An import that skipped rows is not a failure — expired credentials are
	// skipped on purpose and are the majority — so the exit code stays 0 and
	// the report is what carries the detail. What does fail is not reaching the
	// source or the destination, which is handled above.
	return 0
}

// sourceDriver resolves --from-driver, inferring it from the DSN when unset.
//
// The inference covers the two spellings a postgres URL actually has and
// nothing else. Everything unrecognized is a SQLite path rather than an error,
// because that is what a path looks like; a wrong guess fails at the ping with
// the driver named, which is a better diagnosis than a syntax rule the operator
// has to satisfy before they can find out.
func sourceDriver(flagValue, dsn string) (store.Dialect, error) {
	switch strings.ToLower(strings.TrimSpace(flagValue)) {
	case "":
	case "postgres", "postgresql", "pg":
		return store.DialectPostgres, nil
	case "sqlite", "sqlite3":
		return store.DialectSQLite, nil
	default:
		return "", fmt.Errorf("--from-driver: want postgres or sqlite, got %q", flagValue)
	}
	lower := strings.ToLower(dsn)
	if strings.HasPrefix(lower, "postgres://") || strings.HasPrefix(lower, "postgresql://") {
		return store.DialectPostgres, nil
	}
	return store.DialectSQLite, nil
}
