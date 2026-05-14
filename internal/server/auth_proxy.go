package server

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// newGotrueProxy builds the /auth/v1/* reverse proxy. Path is stripped
// of the /auth/v1 prefix before being forwarded — gotrue sees the
// request at its own root (e.g. /authorize, /.well-known/jwks.json).
//
// Why in-binary instead of Caddy/Nginx: one less moving part for the
// self-host operator. D13 (SKY-256) can refactor to a fronting Caddy
// once the container packaging lands. For v1, the TF binary fronts
// the only HTTPS-bearing endpoint anyway.
//
// We pin the Host header to the gotrue upstream's host rather than
// passing the client's Host through. GoTrue's config validates
// API_EXTERNAL_URL against incoming Host on some flows; rewriting
// here is the safe default and matches the GoTrue self-host docs.
func newGotrueProxy(rawURL string) (http.Handler, error) {
	target, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse %q: %w", rawURL, err)
	}
	if target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("gotrue url missing scheme or host: %q", rawURL)
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// SetURL points the outgoing request at the target's
			// scheme + host (preserving query string). We strip the
			// /auth/v1 prefix from the outgoing path so GoTrue sees
			// requests at its own root (/authorize, /token,
			// /.well-known/jwks.json).
			pr.SetURL(target)
			pr.Out.URL.Path = strings.TrimPrefix(pr.Out.URL.Path, "/auth/v1")
			if pr.Out.URL.Path == "" {
				pr.Out.URL.Path = "/"
			}
			if pr.Out.URL.RawPath != "" {
				pr.Out.URL.RawPath = strings.TrimPrefix(pr.Out.URL.RawPath, "/auth/v1")
			}
			// Pin Host to the upstream — GoTrue validates some flows
			// against the API_EXTERNAL_URL host, but our compose wires
			// it to the public host, so passing the gotrue:9999 host
			// matches the internal API expectations.
			pr.Out.Host = target.Host
		},
	}
	return proxy, nil
}
