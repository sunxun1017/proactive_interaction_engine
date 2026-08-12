// Package lifecycle owns bounded runtime queues, one opaque application wakeup
// timer, and the process lifecycle around the application engine. It runs at
// most one observation or wakeup work at a time and gives software controls a
// P0 cancel, stop, and join path without interpreting application semantics.
package lifecycle
