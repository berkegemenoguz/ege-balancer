// Package proxy is the proxy core: it forwards requests to the backend chosen
// by the balancer, and applies the retry, circuit breaker and failure policy
// logic on top of it.
package proxy
