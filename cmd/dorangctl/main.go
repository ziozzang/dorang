// Command dorangctl is dorang's operator CLI.
//
// Every subcommand answers through the same code the server runs: the price
// preview uses internal/pricing's own evaluator, the configuration check uses
// internal/config's validator, the catalog commands use pkg/catalog's loader
// and linter, and the key commands use internal/store. DESIGN §8.4 asks for one
// answer rather than three, and that only holds if there is one implementation.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"

	"github.com/ziozzang/dorang/internal/config"
)

var version = ""

// EnvConfigPath supplies the default --config for every subcommand that needs a
// configuration, so the CLI and the server read the same file by default.
const EnvConfigPath = "DORANG_CONFIG"

const defaultConfigPath = "dorang.yaml"

// env carries the process's streams so every subcommand is testable without
// touching os.Stdout.
type env struct {
	stdout io.Writer
	stderr io.Writer
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	e := env{stdout: stdout, stderr: stderr}
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		usage(stdout)
		return 0
	case "-v", "--version", "version":
		fmt.Fprintln(stdout, versionString())
		return 0
	case "config":
		return e.runConfig(args[1:])
	case "catalog":
		return e.runCatalog(args[1:])
	case "price":
		return e.runPrice(args[1:])
	case "import":
		return e.runImport(args[1:])
	case "key":
		return e.runKey(args[1:])
	case "migrate":
		return e.runMigrate(args[1:])
	case "health":
		return e.runHealth(args[1:])
	}
	fmt.Fprintf(stderr, "dorangctl: unknown command %q\n", args[0])
	usage(stderr)
	return 2
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: dorangctl <command> [flags]

commands:
  config lint [file...]        validate a configuration file and the catalogs it names
  config check [file]          alias for config lint
  catalog explain <kind> <model>
                               show which file set each resolved field
  catalog unverified           list models whose capabilities are not verified
  price <model> --input N --output N
                               preview the cost of one request, rule by rule
  import config <file>         convert a foreign proxy configuration, with warnings
  key create                   issue an api key and print it once
  key list                     list issued keys
  key revoke <id>              block a key
  migrate                      apply database migrations and exit
  health [--addr URL] [--ready]
                               probe a running gateway; exit 0 only if healthy.
                               This is what the container image's HEALTHCHECK runs

common flags:
  --config <path>              configuration file (default $DORANG_CONFIG or dorang.yaml)
`)
}

// newFlagSet builds a subcommand flag set that reports errors to stderr.
func newFlagSet(name string, e env) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	return fs
}

// configPathFlag registers --config with the shared default.
func configPathFlag(fs *flag.FlagSet) *string {
	def := os.Getenv(EnvConfigPath)
	if def == "" {
		def = defaultConfigPath
	}
	return fs.String("config", def, "configuration file")
}

// loadConfigFile loads and validates a configuration, reporting every problem.
func (e env) loadConfigFile(path string) (*config.Config, bool) {
	cfg, err := config.Load(path)
	if err == nil {
		return cfg, true
	}
	e.reportProblems(path, err)
	return nil, false
}

func (e env) reportProblems(path string, err error) {
	problems := config.Problems(err)
	if len(problems) == 0 {
		fmt.Fprintf(e.stderr, "dorangctl: %s: %v\n", path, err)
		return
	}
	fmt.Fprintf(e.stderr, "dorangctl: %s: %d problem(s):\n", path, len(problems))
	for _, p := range problems {
		fmt.Fprintf(e.stderr, "  %s\n", p.Error())
	}
}

func (e env) fail(format string, args ...any) int {
	fmt.Fprintf(e.stderr, "dorangctl: "+format+"\n", args...)
	return 1
}

func versionString() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "devel"
}

// splitLeadingArgs separates leading positional arguments from flags.
//
// Go's flag package stops at the first non-flag token, which would make
// `dorangctl price model-x --input 10` parse nothing. Operators write the
// subject first and the options after, so the leading positionals are lifted out
// before parsing and the remainder is handed to the flag set.
func splitLeadingArgs(args []string) (pos, flags []string) {
	i := 0
	for i < len(args) && (args[i] == "" || args[i][0] != '-') {
		i++
	}
	return args[:i], args[i:]
}

// isNotExist keeps the "no such file" case readable at the call sites.
func isNotExist(err error) bool { return errors.Is(err, os.ErrNotExist) }
