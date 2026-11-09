// Package claudeservertool implements Anthropic server-side tools at the
// response boundary. The unsupported implementation returns unavailable results.
package claudeservertool

import (
	"fmt"
	"net/http"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	jsonbody "github.com/ccat3z/caddy-llm/jsonbody"
)

func init() {
	caddy.RegisterModule(ClaudeServerTool{})
	httpcaddyfile.RegisterHandlerDirective("claudeservertool", parseCaddyfile)
	// Wrap translators: response bytes return in reverse handler order.
	httpcaddyfile.RegisterDirectiveOrder("claudeservertool", httpcaddyfile.Before, "rewrite")
}

// ClaudeServerTool handles actual server-tool calls, not capability declarations.
// Place it before a Claude protocol translator in an explicitly ordered route.
type ClaudeServerTool struct {
	Impl string `json:"impl,omitempty"`
}

func (ClaudeServerTool) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.claudeservertool",
		New: func() caddy.Module { return new(ClaudeServerTool) },
	}
}

func (h *ClaudeServerTool) Provision(_ caddy.Context) error {
	if h.Impl == "" {
		h.Impl = "unsupported"
	}
	if h.Impl != "unsupported" {
		return fmt.Errorf("claudeservertool: unsupported impl %q", h.Impl)
	}
	return nil
}

func (h *ClaudeServerTool) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if r.Method != http.MethodPost {
		return next.ServeHTTP(w, r)
	}
	body, err := jsonbody.FromBody(r)
	if err != nil {
		return next.ServeHTTP(w, r)
	}
	// Declarations only select response handling and supply the schema needed
	// by function-calling translators. Merely declaring a tool never fails.
	root := body.LazyJsonNode
	prepared, tools := prepareRequest(&root)
	if len(tools) > 0 {
		jsonbody.SetRequestBody(r, &jsonbody.JsonReqBody{LazyJsonNode: *prepared})
	}
	rw := newResponseWriter(w, tools)
	if err := next.ServeHTTP(rw, r); err != nil {
		return err
	}
	return rw.finish()
}

func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var handler ClaudeServerTool
	for h.Next() {
		if h.NextArg() {
			return nil, h.ArgErr()
		}
		seen := false
		for h.NextBlock(0) {
			if h.Val() != "impl" {
				return nil, h.Errf("unrecognized claudeservertool option %q", h.Val())
			}
			if seen {
				return nil, h.Err("duplicate impl option")
			}
			seen = true
			if !h.AllArgs(&handler.Impl) {
				return nil, h.ArgErr()
			}
		}
	}
	return &handler, nil
}

var (
	_ caddy.Provisioner           = (*ClaudeServerTool)(nil)
	_ caddyhttp.MiddlewareHandler = (*ClaudeServerTool)(nil)
)
