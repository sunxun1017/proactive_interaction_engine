// Package registry implements the in-process gRPC capability provider registry
// boundary. It validates generated transport messages, owns provider leases,
// and exposes immutable application readiness snapshots without selecting a
// provider or evaluating a scenario.
package registry
