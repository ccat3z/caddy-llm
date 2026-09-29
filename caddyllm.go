// Package caddyllm aggregates all caddy-llm modules so the module root can be
// imported wholesale, e.g. via `xcaddy build --with github.com/ccat3z/caddy-llm=.`.
package caddyllm

import (
	// caddy-llm modules.
	_ "github.com/ccat3z/caddy-llm/claudetoopenai"
	_ "github.com/ccat3z/caddy-llm/llmroute"
	_ "github.com/ccat3z/caddy-llm/trace"
)
