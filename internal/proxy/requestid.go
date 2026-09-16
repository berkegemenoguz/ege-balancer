package proxy

import (
	"context"
	"crypto/rand"
	"net/http"
)

// requestIDHeader carries the identifier of a request: in from a client that
// already has one, on to the backend, and back in the client's response.
const requestIDHeader = "X-Request-Id"

// maxRequestIDLength bounds an identifier taken from a client, so that one
// header cannot fill the logs of the balancer and of every backend behind it.
const maxRequestIDLength = 64

// requestIDKey addresses a request's identifier inside its context.
type requestIDKey struct{}

// withRequestID gives every request an identifier, returns it to the client,
// and carries it on the context for the logs and for the backend.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := requestIDOf(r)
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

// requestIDOf returns the identifier the client sent when it can be logged and
// forwarded as it is, and a fresh one otherwise. A client's own identifier is
// kept, so that a trace which started before the balancer is not broken here.
func requestIDOf(r *http.Request) string {
	if sent := r.Header.Get(requestIDHeader); usableRequestID(sent) {
		return sent
	}
	return rand.Text()
}

// usableRequestID reports whether an identifier from a client is printable
// ASCII and within the length bound.
func usableRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLength {
		return false
	}
	for _, c := range []byte(id) {
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

// requestIDFrom returns the identifier of the request being served, or an empty
// string when there is none, as when a test drives the core directly.
func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}
