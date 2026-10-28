// Package llmalias provides the llm_alias handler: rewrite the request's
// model name when it matches a regular expression. The body's other bytes
// are untouched, and the handler is request-format-agnostic — it only looks
// at the top-level "model" member, so it works on Claude-shaped and
// OpenAI-shaped bodies alike.
package llmalias

import (
	"fmt"
	caddyllm "github.com/ccat3z/caddy-llm/jsonbody"
	"net/http"
	"regexp"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func init() {
	caddy.RegisterModule(LLMAlias{})
	httpcaddyfile.RegisterHandlerDirective("llm_alias", parseCaddyfile)
	// The rewrite must happen before anything matches on or translates the
	// model name (llm_route, claude2openai). Plugins cannot order relative
	// to another plugin's directive, so anchor earlier in the standard
	// pipeline: Before "rewrite" is deterministic regardless of plugin init
	// order.
	httpcaddyfile.RegisterDirectiveOrder("llm_alias", httpcaddyfile.Before, "rewrite")
}

// LLMAlias rewrites a request body's model name: when the model matches
// Find, it is replaced by Replace (with capture-group support). The
// matching follows Caddy's built-in regexp conventions: RE2 syntax,
// evaluated unanchored — anchor with ^...$ to match the whole model name.
type LLMAlias struct {
	// The regular expression to find, in RE2 syntax, which is the same
	// general syntax used by Go, Perl, and Python. Unanchored: use ^...$ to
	// match the whole model name. For details, see
	// [Go's regexp package](https://golang.org/pkg/regexp/).
	Find string `json:"find,omitempty"`

	// The string to replace the match with. Supports regular expression
	// capture groups (e.g. $1).
	Replace string `json:"replace,omitempty"`

	compiled *regexp.Regexp
}

// CaddyModule returns the Caddy module information.
func (LLMAlias) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.llm_alias",
		New: func() caddy.Module { return new(LLMAlias) },
	}
}

// Provision validates the configuration.
func (a *LLMAlias) Provision(_ caddy.Context) error {
	if a.Find == "" {
		return fmt.Errorf("llm_alias requires a find pattern")
	}
	re, err := regexp.Compile(a.Find)
	if err != nil {
		return fmt.Errorf("compiling llm_alias regexp %s: %v", a.Find, err)
	}
	a.compiled = re
	return nil
}

// Interface guard
var _ caddyhttp.MiddlewareHandler = (*LLMAlias)(nil)

func (a *LLMAlias) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if r.Method != http.MethodPost {
		return next.ServeHTTP(w, r)
	}
	body, err := caddyllm.FromBody(r)
	if err != nil {
		// Not this handler's business: FromBody re-installed the raw bytes,
		// so the rest of the chain sees the request untouched and rejects it
		// where appropriate.
		return next.ServeHTTP(w, r)
	}
	model, ok := body.Get("model").String()
	if !ok || !a.compiled.MatchString(model) {
		return next.ServeHTTP(w, r)
	}
	if replaced := a.compiled.ReplaceAllString(model, a.Replace); replaced != model {
		variant := body.With("model", replaced)
		caddyllm.SetRequestBody(r, &caddyllm.JsonReqBody{LazyJsonNode: *variant})
	}
	return next.ServeHTTP(w, r)
}

// ---------- Caddyfile ----------

// llm_alias <find> <replace>
func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var a LLMAlias
	for h.Next() {
		if !h.NextArg() {
			return nil, h.ArgErr()
		}
		a.Find = h.Val()
		if !h.NextArg() {
			return nil, h.ArgErr()
		}
		a.Replace = h.Val()
		if h.NextArg() {
			return nil, h.ArgErr()
		}
	}
	return &a, nil
}
