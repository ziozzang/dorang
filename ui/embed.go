// Package ui holds the administration UI's assets, embedded in the binary.
//
// DESIGN §11.3 requires the admin UI to work offline with no external assets.
// That is enforced here structurally: the whole UI is a byte slice compiled
// into the executable, so there is no configuration under which it can reach a
// CDN, and no deployment in which it can be half-installed.
//
// This package deliberately contains no handlers and no data access. It hands
// out two filesystems and nothing else, so that the screens can be rendered by
// whoever has the data — internal/admin — without ui growing a dependency on
// the store, the router, or anything else that changes on a different schedule.
//
// Server-rendered session forms and embedded JavaScript provide bilingual
// navigation, theme preferences, filtering and live telemetry over SSE.
package ui

import (
	"embed"
	"io/fs"
)

//go:embed assets templates
var files embed.FS

// Assets returns the static asset tree: stylesheets and JavaScript modules.
//
// The returned filesystem is rooted at the asset directory, so "style.css"
// addresses the stylesheet. It is read-only and safe for concurrent use.
func Assets() fs.FS { return must(fs.Sub(files, "assets")) }

// Templates returns the html/template sources, rooted at the template
// directory. Every page template is "layout.html" plus exactly one page file,
// so a caller parses them in pairs rather than all at once.
func Templates() fs.FS { return must(fs.Sub(files, "templates")) }

// Pages names the page templates, without the ".html" suffix. Each one is
// parsed together with layout.html and defines a "content" block.
//
// "newkey", "confirm" and "secret" are the three pages the credential
// lifecycle needs beyond the table: the form that mints, the interstitial that
// names a key before something irreversible happens to it, and the one page in
// dorang that ever displays a plaintext credential.
func Pages() []string {
	return []string{"setup", "console", "keys", "users", "models", "usage", "monitoring", "login", "message", "newkey", "editkey", "edituser", "editteam", "confirm", "secret"}
}

func must(f fs.FS, err error) fs.FS {
	if err != nil {
		// Unreachable: the directories are embedded above, so fs.Sub can only
		// fail if this file and the embed directive disagree, which does not
		// survive compilation.
		panic("ui: " + err.Error())
	}
	return f
}
