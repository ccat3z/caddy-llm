// Package imagefallback provides the imagefallback handler: when a Claude
// Messages request carries an image block and the requested model matches a
// find regexp, the model name is rewritten to a replace template (a
// vision-capable fallback). The image itself passes through untouched — the
// fallback model reads it.
package imagefallback

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
	caddy.RegisterModule(ImageFallback{})
	httpcaddyfile.RegisterHandlerDirective("imagefallback", parseCaddyfile)
	// The rewrite must happen while the body is still Claude-shaped, i.e.
	// before claude2openai translates it (and before llm_route matches on
	// the model). Plugins cannot order relative to another plugin's
	// directive, so anchor earlier in the standard pipeline than either:
	// Before "rewrite" is deterministic regardless of plugin init order.
	httpcaddyfile.RegisterDirectiveOrder("imagefallback", httpcaddyfile.Before, "rewrite")
}

// ImageFallback rewrites an Anthropic Messages request's model to a
// vision-capable fallback when the request contains an image block. Text-only
// upstream models reject any request whose history mentions an image, so the
// condition is "an image block anywhere in messages" — not just in the newest
// turn. Every other byte of the request (the image included) is preserved;
// requests without images, or whose model does not match, pass through with
// their original body untouched.
type ImageFallback struct {
	// The regular expression to find in the model name, in RE2 syntax.
	// Unanchored, like Caddy's built-in regexp modules: anchor with ^...$ to
	// match the whole model name. For details, see
	// [Go's regexp package](https://golang.org/pkg/regexp/).
	Find string `json:"find,omitempty"`

	// The string to replace the match with — the fallback model. Supports
	// regular expression capture groups (e.g. $1).
	Replace string `json:"replace,omitempty"`

	re *regexp.Regexp
}

// CaddyModule returns the Caddy module information.
func (ImageFallback) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.imagefallback",
		New: func() caddy.Module { return new(ImageFallback) },
	}
}

// Provision validates the configuration.
func (f *ImageFallback) Provision(_ caddy.Context) error {
	if f.Replace == "" {
		return fmt.Errorf("imagefallback requires a replace (fallback) model name")
	}
	re, err := regexp.Compile(f.Find)
	if err != nil {
		return fmt.Errorf("compiling imagefallback regexp %s: %v", f.Find, err)
	}
	f.re = re
	return nil
}

// Interface guard
var _ caddyhttp.MiddlewareHandler = (*ImageFallback)(nil)

func (f *ImageFallback) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
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
	if !requestHasImage(body.Get("messages")) {
		return next.ServeHTTP(w, r)
	}
	model, _ := body.Get("model").String()
	if !f.re.MatchString(model) {
		return next.ServeHTTP(w, r)
	}
	variant := body.With("model", f.re.ReplaceAllString(model, f.Replace))
	caddyllm.SetRequestBody(r, &caddyllm.JsonReqBody{LazyJsonNode: *variant})
	return next.ServeHTTP(w, r)
}

// requestHasImage reports whether any message's content contains an image
// block. tool_result content is itself a content array, so the recursion
// covers images returned by tools.
func requestHasImage(n *caddyllm.LazyJsonNode) bool {
	if n.Type() != caddyllm.TypeArray {
		return false
	}
	messages, _ := n.List()
	for _, m := range messages {
		if contentHasImage(m.Get("content")) {
			return true
		}
	}
	return false
}

// contentHasImage reports whether a content array (or a bare string, which
// never carries images) contains an image block.
func contentHasImage(n *caddyllm.LazyJsonNode) bool {
	if n.Type() != caddyllm.TypeArray {
		return false
	}
	blocks, _ := n.List()
	for _, b := range blocks {
		blockType, _ := b.Get("type").String()
		if blockType == "image" {
			return true
		}
		if blockType == "tool_result" && contentHasImage(b.Get("content")) {
			return true
		}
	}
	return false
}

// ---------- Caddyfile ----------

// imagefallback <find> <replace>
func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var f ImageFallback
	for h.Next() {
		if !h.NextArg() {
			return nil, h.ArgErr()
		}
		f.Find = h.Val()
		if !h.NextArg() {
			return nil, h.ArgErr()
		}
		f.Replace = h.Val()
		if h.NextArg() {
			return nil, h.ArgErr()
		}
	}
	return &f, nil
}
