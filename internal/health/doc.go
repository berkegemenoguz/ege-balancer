// Package health implements active (periodic HTTP probe) and passive
// (consecutive failure counter) health checking, and decides when a backend
// leaves or rejoins the pool.
package health
