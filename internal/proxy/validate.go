package proxy

import "net/http"

// validateRequest rejects requests whose framing headers are ambiguous, before
// any backend sees them. Go's own server already refuses the clearest cases;
// this repeats the check so that the proxy does not depend on that and covers
// requests reaching the handler by other paths.
func validateRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if suspiciousFraming(r) {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// suspiciousFraming reports header combinations that let a request be read as
// two different requests by the proxy and the backend (request smuggling).
func suspiciousFraming(r *http.Request) bool {
	lengths := r.Header.Values("Content-Length")
	if len(lengths) > 1 {
		return true
	}
	// A body framed twice, by length and by chunking, is ambiguous.
	return len(lengths) == 1 && len(r.Header.Values("Transfer-Encoding")) > 0
}
