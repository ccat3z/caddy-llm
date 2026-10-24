// Package imagefallback provides the imagefallback handler: when a Claude
// Messages request carries an image block and the requested model matches a
// pattern, the model name is rewritten to a vision-capable fallback. The
// image itself passes through untouched — the fallback model reads it.
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
	// Pattern matches the requested model name (anchored regexp).
	Pattern string `json:"pattern,omitempty"`
	// Fallback is the model name the request is rewritten to.
	Fallback string `json:"fallback,omitempty"`

	re *regexp.Regexp `json:"-"`
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
	if f.Fallback == "" {
		return fmt.Errorf("imagefallback requires a fallback model name")
	}
	re, err := regexp.Compile("^(?:" + f.Pattern + ")$")
	if err != nil {
		return fmt.Errorf("imagefallback pattern %q: %v", f.Pattern, err)
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
	variant := body.With("model", f.Fallback)
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

// imagefallback <regex match model name> <fallback model name>
func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var f ImageFallback
	for h.Next() {
		if !h.NextArg() {
			return nil, h.ArgErr()
		}
		f.Pattern = h.Val()
		if !h.NextArg() {
			return nil, h.ArgErr()
		}
		f.Fallback = h.Val()
		if h.NextArg() {
			return nil, h.ArgErr()
		}
	}
	return &f, nil
}
