package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ziozzang/dorang/internal/app"
	"github.com/ziozzang/dorang/internal/store"
)

// runKey implements `key create|list|revoke`.
func (e env) runKey(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(e.stderr, "usage: dorangctl key create|list|revoke")
		return 2
	}
	switch args[0] {
	case "create":
		return e.keyCreate(args[1:])
	case "list":
		return e.keyList(args[1:])
	case "revoke":
		return e.keyRevoke(args[1:])
	}
	return e.fail("unknown key subcommand %q", args[0])
}

// keyCreate issues a credential and prints it exactly once.
//
// The token is generated here and hashed by the store under dorang_v1: what is
// persisted is HMAC-SHA256(pepper, token), so a stolen database is not offline
// attackable (DESIGN §2.4). Nothing recoverable is kept, which is why the token
// is printed with a warning rather than being retrievable later.
func (e env) keyCreate(args []string) int {
	fs := newFlagSet("key create", e)
	cfgPath := configPathFlag(fs)
	var (
		alias    = fs.String("alias", "", "display alias")
		user     = fs.String("user", "", "owning user id")
		team     = fs.String("team", "", "owning team id")
		models   = fs.String("models", "", "comma-separated model allow-list; empty allows all")
		routes   = fs.String("routes", "", "comma-separated route allow-list; empty allows all")
		budget   = fs.Float64("budget-usd", 0, "spend ceiling in USD; 0 means no ceiling")
		rpm      = fs.Int64("rpm", 0, "requests-per-minute ceiling; 0 means none")
		tpm      = fs.Int64("tpm", 0, "tokens-per-minute ceiling; 0 means none")
		parallel = fs.Int64("max-parallel", 0, "concurrency ceiling; 0 means none")
		ttl      = fs.Duration("ttl", 0, "expire the key after this long; 0 never expires")
		class    = fs.String("priority-class", "", "priority class")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, ok := e.loadConfigFile(*cfgPath)
	if !ok {
		return 1
	}
	ctx := context.Background()
	st, err := app.OpenStore(ctx, cfg, false)
	if err != nil {
		return e.fail("%v", err)
	}
	defer st.Close()

	token, err := newToken()
	if err != nil {
		return e.fail("%v", err)
	}
	k := &store.APIKey{
		KeyAlias:      *alias,
		UserID:        *user,
		TeamID:        *team,
		Models:        splitComma(*models),
		AllowedRoutes: splitComma(*routes),
		RPMLimit:      optInt(*rpm),
		TPMLimit:      optInt(*tpm),
		MaxParallel:   optInt(*parallel),
		PriorityClass: *class,
		Source:        "native",
	}
	if *budget > 0 {
		nano := int64(*budget * 1e9)
		k.MaxBudgetNano = &nano
	}
	if *ttl > 0 {
		k.ExpiresAt = time.Now().Add(*ttl)
	}
	if err := st.NewAPIKeyFromToken(token, k); err != nil {
		return e.fail("%v", err)
	}
	if err := st.InsertAPIKey(ctx, k); err != nil {
		return e.fail("%v", err)
	}
	fmt.Fprintf(e.stdout, "%s\n", token)
	fmt.Fprintf(e.stderr, "key %s created (label %s). "+
		"The token above is shown once and is not recoverable.\n", k.ID, k.KeyLabel)
	return 0
}

// keyList lists issued keys.
//
// internal/store models the six ledger queries DESIGN §9.3 names and nothing
// else, so there is no ListAPIKeys to call. Only the ID SELECT goes through
// Store.DB — which the store exports for operations it does not model — and
// every row is then read back through GetAPIKey, so the column list, the
// nullable handling and the dialect's placeholder syntax stay the store's
// business rather than this file's.
func (e env) keyList(args []string) int {
	fs := newFlagSet("key list", e)
	cfgPath := configPathFlag(fs)
	limit := fs.Int("limit", 100, "maximum rows")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, ok := e.loadConfigFile(*cfgPath)
	if !ok {
		return 1
	}
	ctx := context.Background()
	st, err := app.OpenStore(ctx, cfg, false)
	if err != nil {
		return e.fail("%v", err)
	}
	defer st.Close()

	ids, err := listKeyIDs(ctx, st, *limit)
	if err != nil {
		return e.fail("%v", err)
	}
	if len(ids) == 0 {
		fmt.Fprintln(e.stderr, "no keys")
		return 0
	}

	tw := tabwriter.NewWriter(e.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tLABEL\tALIAS\tUSER\tTEAM\tSCHEME\tSTATE\tSPEND(USD)\tBUDGET(USD)\tSOURCE\tCREATED")
	now := time.Now()
	for _, id := range ids {
		k, err := st.GetAPIKey(ctx, id)
		if err != nil {
			return e.fail("%v", err)
		}
		state := "active"
		switch {
		case k.Blocked:
			state = "blocked"
		case k.Expired(now):
			state = "expired"
		}
		budget := "-"
		if k.MaxBudgetNano != nil {
			budget = formatNano(*k.MaxBudgetNano)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			k.ID, k.KeyLabel, dash(k.KeyAlias), dash(k.UserID), dash(k.TeamID),
			string(k.HashScheme), state, formatNano(k.SpendNano), budget, k.Source,
			k.CreatedAt.UTC().Format(time.RFC3339))
	}
	_ = tw.Flush()
	return 0
}

// listKeyIDs is the one query internal/store does not offer.
func listKeyIDs(ctx context.Context, st *store.Store, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := st.DB().QueryContext(ctx,
		"SELECT id FROM api_keys ORDER BY created_at DESC LIMIT "+strconv.Itoa(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// keyRevoke blocks a key.
//
// Blocking rather than deleting is deliberate: the ledger references the key id,
// and a deleted row would turn every historical request into an orphan. An
// expired or blocked key must stay present and be refused as such (R1-A), never
// disappear and be refused as unknown.
func (e env) keyRevoke(args []string) int {
	fs := newFlagSet("key revoke", e)
	cfgPath := configPathFlag(fs)
	pos, flags := splitLeadingArgs(args)
	if err := fs.Parse(flags); err != nil {
		return 2
	}
	rest := append(pos, fs.Args()...)
	if len(rest) != 1 {
		fmt.Fprintln(e.stderr, "usage: dorangctl key revoke <id>")
		return 2
	}
	cfg, ok := e.loadConfigFile(*cfgPath)
	if !ok {
		return 1
	}
	ctx := context.Background()
	st, err := app.OpenStore(ctx, cfg, false)
	if err != nil {
		return e.fail("%v", err)
	}
	defer st.Close()

	if _, err := st.GetAPIKey(ctx, rest[0]); err != nil {
		return e.fail("%v", err)
	}
	// [store.Store.RevokeKey], not a hand-written UPDATE. This used to reach for
	// st.DB() and write `blocked = 1` itself, which made it a FOURTH place that
	// knew how to stop a key serving, and — the part that mattered — a place
	// that could not publish the change because it did not have the index keys
	// the invalidation names. RevokeKey returns them.
	lookups, err := st.RevokeKey(ctx, rest[0])
	if err != nil {
		return e.fail("%v", err)
	}

	// The revocation is PUBLISHED, so a running fleet honours it within
	// `auth.revocation.poll` plus one store round trip (DESIGN §11.2c) rather
	// than when each node's credential cache happens to expire. This process has
	// no snapshot of its own to drop — it is a CLI, not a gateway — so the
	// durable message is the whole of its half, and the failure is reported
	// rather than swallowed: the block is durable either way, and an operator
	// who is told nothing would assume the fast path took.
	if _, err := st.PublishInvalidation(ctx, store.KeyInvalidation{
		KeyID: rest[0], Lookups: lookups, Cause: "revoked", CreatedAt: time.Now(),
	}); err != nil {
		fmt.Fprintf(e.stdout, "key %s revoked\n", rest[0])
		fmt.Fprintf(e.stderr, "the key is blocked, but the invalidation could not be published "+
			"(%v); a running gateway will pick it up when its credential cache expires "+
			"(auth.revocation.entry_ttl) or on SIGHUP\n", err)
		return 1
	}
	fmt.Fprintf(e.stdout, "key %s revoked\n", rest[0])
	fmt.Fprintln(e.stderr, "published; a running gateway stops serving this key within "+
		"auth.revocation.poll plus one store round trip")
	return 0
}

// runMigrate applies migrations and exits.
func (e env) runMigrate(args []string) int {
	fs := newFlagSet("migrate", e)
	cfgPath := configPathFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, ok := e.loadConfigFile(*cfgPath)
	if !ok {
		return 1
	}
	ctx := context.Background()
	// SkipMigrate so the report comes from the explicit call rather than from a
	// migration that already happened silently inside Open.
	st, err := app.OpenStore(ctx, cfg, true)
	if err != nil {
		return e.fail("%v", err)
	}
	defer st.Close()

	before, _ := st.SchemaVersion(ctx)
	rep, err := st.Migrate(ctx)
	if err != nil {
		return e.fail("%v", err)
	}
	after, _ := st.SchemaVersion(ctx)
	if len(rep.Applied) == 0 {
		fmt.Fprintf(e.stdout, "schema is already at version %d; nothing to apply\n", after)
		return 0
	}
	fmt.Fprintf(e.stdout, "applied %d migration(s), %d -> %d:\n", len(rep.Applied), before, after)
	for _, m := range rep.Applied {
		fmt.Fprintf(e.stdout, "  %d %s\n", m.Version, m.Name)
	}
	return 0
}

// newToken generates a credential. The "sk-" prefix is mandatory: a stored row
// is a hex digest, hex does not start with "sk-", and the gate checks the prefix
// before any lookup so a leaked digest cannot be replayed as the credential it
// stands for (R1-A).
func newToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "sk-" + strings.TrimRight(base64.URLEncoding.EncodeToString(raw[:]), "="), nil
}

func optInt(v int64) *int64 {
	if v <= 0 {
		return nil
	}
	return &v
}
