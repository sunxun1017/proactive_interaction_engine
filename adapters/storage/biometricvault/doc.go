// Package biometricvault stores opaque biometric templates encrypted at rest.
// It is a dedicated adapter boundary: template bytes must never be passed into
// the domain, application services, general transports, or semantic audit.
package biometricvault
