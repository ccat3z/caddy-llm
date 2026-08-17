// Command caddy-llm runs Caddy with all caddy-llm modules built in.
package main

import (
	caddycmd "github.com/caddyserver/caddy/v2/cmd"

	// Caddy core modules.
	_ "github.com/caddyserver/caddy/v2/modules/standard"

	// caddy-llm modules.
	_ "github.com/ccat3z/caddy-llm/internal/all"
)

func main() {
	caddycmd.Main()
}
