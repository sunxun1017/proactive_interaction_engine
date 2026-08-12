// Package episode tracks the active proactive interaction and evaluates
// strongly typed user-feedback events into immutable outcomes. It performs no
// I/O. Tracker owns process-local feedback idempotency history; durable storage
// may be added later outside the real-time path.
package episode
