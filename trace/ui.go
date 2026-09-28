package trace

import (
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/caddyserver/caddy/v2"

	webui "github.com/ccat3z/caddy-llm/web"
)

// uiPrefix is the reserved first path segment under the TraceAPI mount
// where the embedded dashboard is served (e.g. mounted at /llm/traces, the
// UI is at /llm/traces/ui/). It only claims the first segment: detail
// routes are {traceID}/{name} with random 16-hex traceIDs, and stage names
// sit in the second segment, so neither can collide with it.
const uiPrefix = "ui/"

// distFS is the embedded dashboard, or nil when web/dist was never built
// (fresh clone; only the .gitkeep placeholder is embedded).
var distFS = func() fs.FS {
	sub, err := fs.Sub(webui.Dist, "dist")
	if err != nil {
		return nil
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil
	}
	return sub
}()

// notBuiltPage is served at the UI route when the dashboard wasn't built
// into the binary.
const notBuiltPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>caddy-llm traces</title></head>
<body style="font-family:system-ui;max-width:40em;margin:4em auto;line-height:1.6">
<h1>Traces UI not built into this binary</h1>
<p>The dashboard ships as a vite build embedded at compile time. To include it:</p>
<pre style="background:#f4f4f4;padding:1em;border-radius:6px">cd web &amp;&amp; npm install &amp;&amp; npm run build
cd .. &amp;&amp; go build ./cmd/caddy-llm</pre>
</body></html>`

// serveUI serves the embedded dashboard under {mount}/ui/ with SPA
// fallback (unknown paths get index.html).
func (t *TraceAPI) serveUI(w http.ResponseWriter, r *http.Request, rest string) {
	// ServeHTTP preset JSON for the API branches; static files sniff their
	// own type (ServeContent only sniffs when the header is unset).
	w.Header().Del("Content-Type")
	if distFS == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(notBuiltPage))
		return
	}
	// Canonicalize /ui to a trailing slash so relative asset URLs resolve.
	// (rest is slash-trimmed, so /ui and /ui/ look identical; check the
	// actual path.) r.URL.Path is already prefix-stripped here; the
	// client-visible path is the original one, kept in a placeholder.
	if rest == "ui" && !strings.HasSuffix(r.URL.Path, "/") {
		target := r.URL.Path
		if repl, ok := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer); ok {
			if p, ok := repl.GetString("http.request.orig_uri.path"); ok && p != "" {
				target = p
			}
		}
		http.Redirect(w, r, target+"/", http.StatusMovedPermanently)
		return
	}
	name := path.Clean(strings.TrimPrefix(rest, uiPrefix))
	if name == "." || name == "/" || name == "ui" {
		name = "index.html"
	}
	f, err := distFS.Open(name)
	if err != nil {
		// SPA fallback: client-side routes get the app shell.
		name = "index.html"
	} else {
		f.Close()
	}
	// Content-hashed assets are immutable; everything else (index.html)
	// always revalidates so deploys show up immediately.
	if strings.HasPrefix(name, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeFileFS(w, r, distFS, name)
}
