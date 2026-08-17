// Package all imports every caddy-llm module for side-effect registration.
// Import it wherever a complete caddy runtime is needed (integration tests,
// cmd/caddy-llm); individual module packages register only themselves.
package all

import (
	_ "github.com/ccat3z/caddy-llm/internal/claudetoopenai"
	_ "github.com/ccat3z/caddy-llm/internal/tracer"
	_ "github.com/ccat3z/caddy-llm/internal/tracestore"
)
