// Package caddy_llm imports every caddy-llm module for side-effect
// registration. Import it wherever a complete caddy runtime is needed
// (integration tests, cmd/caddy-llm); individual module packages register only
// themselves.
package caddy_llm

import (
	_ "github.com/ccat3z/caddy-llm/claudetoopenai"
	_ "github.com/ccat3z/caddy-llm/llmroute"
	_ "github.com/ccat3z/caddy-llm/trace"
)
