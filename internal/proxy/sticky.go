package proxy

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/berkegemenoguz/ege-balancer/internal/balancer"
	"github.com/berkegemenoguz/ege-balancer/internal/config"
	"github.com/berkegemenoguz/ege-balancer/internal/observability"
)

// Option adjusts the handler New builds.
type Option func(*Core)

// WithStickySecret signs sticky cookies with secret. A cookie signed with a
// secret given this way survives a restart, and is accepted by every balancer
// given the same one. Without it, the handler makes a secret of its own when it
// is built, which lasts as long as the process.
func WithStickySecret(secret []byte) Option {
	return func(c *Core) { c.secret = secret }
}

// newSecret is a random secret for a handler given none: 128 bits, from the
// operating system's source.
func newSecret() []byte {
	return []byte(rand.Text())
}

// cookieState is what a request's sticky cookie turned out to be.
type cookieState int

const (
	// cookieAbsent: the request carried no sticky cookie.
	cookieAbsent cookieState = iota
	// cookieKnown: the cookie names a backend of the pool.
	cookieKnown
	// cookieUnknown: the cookie names none.
	cookieUnknown
)

// pins is the sticky cookie of one configuration: its attributes, and the token
// that names each backend of the pool in it. It is nil when sticky sessions are
// off, and every method treats nil as off.
type pins struct {
	name   string
	maxAge int
	secure bool

	// tokens and backends map each backend to its token and back. They are
	// computed once per configuration, so a request costs a map lookup and no
	// cryptography.
	tokens   map[string]string
	backends map[string]*balancer.Backend
}

// newPins returns the sticky cookie described by sticky for backends, signed
// with secret, or nil when sticky sessions are off.
func newPins(sticky config.Sticky, secret []byte, backends []*balancer.Backend) *pins {
	if sticky.Mode != config.StickyCookie {
		return nil
	}
	p := &pins{
		name:     sticky.Cookie,
		maxAge:   int(time.Duration(sticky.MaxAge).Seconds()),
		secure:   sticky.Secure,
		tokens:   make(map[string]string, len(backends)),
		backends: make(map[string]*balancer.Backend, len(backends)),
	}
	for _, backend := range backends {
		token := pinToken(secret, backend.Addr)
		p.tokens[backend.Addr] = token
		p.backends[token] = backend
	}
	return p
}

// pinToken names a backend in the cookie: the first 128 bits of an HMAC-SHA256
// of its address under the secret, in unpadded base64url. It gives nothing of
// the address away, and a client without the secret cannot make the token of a
// backend it was never sent to.
func pinToken(secret []byte, addr string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(addr))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:16])
}

// read returns the backend a request's cookie names, and what the cookie was.
func (p *pins) read(r *http.Request) (*balancer.Backend, cookieState) {
	if p == nil {
		return nil, cookieAbsent
	}
	cookie, err := r.Cookie(p.name)
	if err != nil {
		return nil, cookieAbsent
	}
	if backend, known := p.backends[cookie.Value]; known {
		return backend, cookieKnown
	}
	return nil, cookieUnknown
}

// renew returns the cookie that pins a client to backend, or nil when the
// client is already pinned there, or sticky sessions are off.
func (p *pins) renew(pinned, backend *balancer.Backend) *http.Cookie {
	if p == nil || backend == pinned {
		return nil
	}
	return &http.Cookie{
		Name:     p.name,
		Value:    p.tokens[backend.Addr],
		Path:     "/",
		MaxAge:   p.maxAge,
		Secure:   p.secure,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
}

// cookieName is the name of the cookie to take off requests on their way to a
// backend, or "" when sticky sessions are off.
func (p *pins) cookieName() string {
	if p == nil {
		return ""
	}
	return p.name
}

// place picks the backend for one attempt: the one the sticky cookie names when
// it is among the candidates, and the algorithm's choice otherwise. A backend
// that is unhealthy, tripped or already tried is not among them, so a retry,
// and a pinned backend leaving the pool, both fall to the algorithm, and the
// answer pins the client to whichever backend gave it. The first attempt also
// records what the cookie did.
func (c *Core) place(active *settings, key string, pinned *balancer.Backend, state cookieState,
	candidates []*balancer.Backend, first bool) (*balancer.Backend, error) {
	if pinned != nil && slices.Contains(candidates, pinned) {
		if first {
			c.metrics.ObserveSticky(observability.StickyPinned)
		}
		return pinned, nil
	}
	if first && active.pins != nil {
		c.metrics.ObserveSticky(stickyResult(state))
	}
	return c.choose(active, key, candidates, first)
}

// stickyResult is how a request whose cookie did not decide its backend is
// counted.
func stickyResult(state cookieState) string {
	switch state {
	case cookieKnown:
		return observability.StickyRepinned
	case cookieUnknown:
		return observability.StickyUnknown
	}
	return observability.StickyNew
}

// removeCookie takes the cookie called name off a request's headers and leaves
// every other cookie exactly as the client sent it. The balancer's cookie is for
// the balancer; a backend has no use for it, and its logs no business holding
// it.
func removeCookie(header http.Header, name string) {
	lines := header.Values("Cookie")
	kept := make([]string, 0, len(lines))
	removed := false
	for _, line := range lines {
		for pair := range strings.SplitSeq(line, ";") {
			pair = strings.TrimSpace(pair)
			if cookieName, _, _ := strings.Cut(pair, "="); strings.TrimSpace(cookieName) == name {
				removed = true
				continue
			}
			if pair != "" {
				kept = append(kept, pair)
			}
		}
	}
	if !removed {
		return
	}
	header.Del("Cookie")
	if len(kept) > 0 {
		header.Set("Cookie", strings.Join(kept, "; "))
	}
}
