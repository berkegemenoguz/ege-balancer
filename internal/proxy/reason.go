package proxy

import (
	"errors"
	"io"
	"net"
	"syscall"
)

// Why an attempt failed, as lb_backend_failures_total reports it. The reasons
// follow the exchange: no connection, no answer, the connection lost before
// the answer, the answer lost part way, and an answer that was itself a
// failure.
const (
	// reasonConnect: no connection could be made — refused, unreachable, or
	// the connect timeout.
	reasonConnect = "connect"
	// reasonTimeout: connected, but no answer began within the response
	// timeout.
	reasonTimeout = "timeout"
	// reasonReset: the connection closed before the answer began.
	reasonReset = "reset"
	// reasonCutOff: the answer began and was broken off.
	reasonCutOff = "cut_off"
	// reason5xx: the backend answered 5xx, which counts as a failure under
	// retry_on_5xx.
	reason5xx = "5xx"
	// reasonOther: anything else.
	reasonOther = "other"
)

// reasons are every reason an attempt can fail for.
var reasons = []string{reasonConnect, reasonTimeout, reasonReset, reasonCutOff, reason5xx, reasonOther}

// failureReason names what went wrong in an attempt that failed before its
// answer began. An answer broken off part way never reaches here: it is
// recognised where the forwarder aborts, and is always reasonCutOff.
func failureReason(err error) string {
	var netErr net.Error
	switch {
	case errors.Is(err, errRetryable5xx):
		return reason5xx
	// Checked before the timeout, which a connect timeout also is.
	case dialFailed(err):
		return reasonConnect
	case errors.As(err, &netErr) && netErr.Timeout():
		return reasonTimeout
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF),
		errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return reasonReset
	}
	return reasonOther
}
