// Package web embeds the built traces dashboard (vite output in dist/) so
// the caddy-llm binary serves it without external files. dist/ is committed
// so xcaddy builds (which only see the git tree) get the UI; rebuild and
// commit it whenever web/src changes:
//
//	cd web && npm install && npm run build
package web

import "embed"

//go:embed all:dist
var Dist embed.FS
