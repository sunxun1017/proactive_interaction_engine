// Package privacyfile persists the complete local privacy-permission snapshot
// in one private, versioned JSON file. It uses optimistic revisions and atomic
// replacement; it does not choose permissions or infer provider health.
package privacyfile
