package main

import (
	"fmt"
	"os"

	"github.com/ziozzang/dorang/internal/app"
	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/pkg/catalog"
	"gopkg.in/yaml.v3"
)

// runConfig implements `config lint` and `config check`.
func (e env) runConfig(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(e.stderr, "usage: dorangctl config lint|check [file...]")
		return 2
	}
	switch args[0] {
	case "lint", "check":
		return e.configLint(args[1:])
	}
	return e.fail("unknown config subcommand %q", args[0])
}

// configLint validates configuration files and the model catalogs they name.
//
// Two different checkers run, because two different files are wrong in two
// different ways: internal/config validates the gateway configuration and
// pkg/catalog lints the model catalog overlays. The catalog layer that
// DORANG_CATALOG_PATH names is included, so this lints what a server would
// actually load rather than only what was typed on the command line.
func (e env) configLint(args []string) int {
	fs := newFlagSet("config lint", e)
	catalogPaths := fs.String("catalog", "", "extra model catalog files or directories, comma separated")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	paths := fs.Args()
	if len(paths) == 0 {
		def := os.Getenv(EnvConfigPath)
		if def == "" {
			def = defaultConfigPath
		}
		paths = []string{def}
	}

	failed := false
	for _, path := range paths {
		// The same policy the server applies to --check: a secret that is not on
		// this machine is a warning, a wrong reference is not.
		cfg, warnings, err := app.CheckConfig(path)
		for _, w := range warnings {
			fmt.Fprintf(e.stderr, "%s: warning: %s\n", path, w)
		}
		if err != nil {
			e.reportProblems(path, err)
			failed = true
			continue
		}
		if cfg == nil {
			fmt.Fprintf(e.stdout, "%s: ok (secrets not readable here, see warnings)\n", path)
			continue
		}
		fmt.Fprintf(e.stdout, "%s: ok — %d provider(s), %d credential(s), %d model group(s)\n",
			path, len(cfg.Providers), len(cfg.Credentials), len(cfg.Models))
	}

	catalogs := splitComma(*catalogPaths)
	catalogs = append(catalogs, catalog.EnvPaths()...)
	if len(catalogs) > 0 {
		problems := catalog.LintFiles(catalogs...)
		for _, p := range problems {
			fmt.Fprintf(e.stderr, "  %s\n", p.String())
		}
		if catalog.HasErrors(problems) {
			failed = true
		} else {
			fmt.Fprintf(e.stdout, "model catalog: ok — %d file layer(s), %d note(s)\n",
				len(catalogs), len(problems))
		}
	}
	if failed {
		return 1
	}
	return 0
}

// runImport implements `import config`.
func (e env) runImport(args []string) int {
	if len(args) < 2 || args[0] != "config" {
		fmt.Fprintln(e.stderr, "usage: dorangctl import config <file>")
		return 2
	}
	path := args[1]
	data, err := os.ReadFile(path)
	if err != nil {
		if isNotExist(err) {
			return e.fail("no such file: %s", path)
		}
		return e.fail("%v", err)
	}
	cfg, warnings, err := config.ImportProxyConfig(data)
	if err != nil {
		return e.fail("%s: %v", path, err)
	}
	// Warnings go to stderr so the converted configuration on stdout can be
	// redirected into a file without them landing in it. Every one of them is
	// something the import could not resolve rather than something it guessed:
	// DESIGN §2.4 records that guessing is what makes an import untrustworthy.
	for _, w := range warnings {
		fmt.Fprintf(e.stderr, "warning: %s\n", w.String())
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return e.fail("%v", err)
	}
	fmt.Fprintf(e.stderr, "# imported %s: %d provider(s), %d credential(s), %d model group(s), %d warning(s)\n",
		path, len(cfg.Providers), len(cfg.Credentials), len(cfg.Models), len(warnings))
	_, _ = e.stdout.Write(out)
	return 0
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if p := s[start:i]; p != "" {
				out = append(out, p)
			}
			start = i + 1
		}
	}
	return out
}
