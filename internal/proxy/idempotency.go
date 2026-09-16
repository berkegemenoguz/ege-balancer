package proxy

import (
	"errors"
	"net"
	"net/http"
)

// idempotent reports whether sending a method twice has the same effect as
// sending it once, as RFC 9110 defines it. A method we do not know is assumed
// not to be.
func idempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete,
		http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

// retryable reports whether a failed attempt may be offered to another backend.
//
// An idempotent request always may. A request that is not — POST, PATCH, or an
// unknown method — may only when nothing can have acted on it yet, which is
// what a failed dial means: no connection was made, so no backend saw the
// request. Once it is on the wire the backend may have carried it out and
// answered into a connection that then broke, and sending it again would repeat
// the effect: a second order, a second payment.
func retryable(method string, err error) bool {
	return idempotent(method) || dialFailed(err)
}

// dialFailed reports whether err comes from failing to open the connection.
func dialFailed(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}
