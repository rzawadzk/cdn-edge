package middleware

import (
	"net/http"
	"strings"
)

// HTTPSRedirect returns an HTTP handler that redirects all requests to HTTPS.
// tlsHost is the hostname:port to redirect to. If empty, uses the request host
// (with the port stripped).
func HTTPSRedirect(tlsHost string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := tlsHost
		if host == "" {
			host = r.Host
			// Strip port if present.
			if i := strings.LastIndex(host, ":"); i > 0 {
				host = host[:i]
			}
		}
		target := "https://" + host + r.URL.RequestURI()
		http.Redirect(w, r, target, http.StatusMovedPermanently)
	})
}
