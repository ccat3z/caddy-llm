// Package web embeds the built traces dashboard (vite output in dist/) so
// the caddy-llm binary serves it without external files. Build it first:
//
//	cd web && npm install && npm run build
//
// dist/ is gitignored except for a .gitkeep placeholder: fresh clones build
// fine, and the UI route serves a "not built" hint page until dist exists.
package web

import "embed"

//go:embed all:dist
var Dist embed.FS
