# ADR 0003: Use Secret Service for the Biometric Master Key

## Status

Accepted

## Context

The Ubuntu desktop product needs a production AES-256 master key for the
encrypted biometric catalog and template vault. Storing that key beside the
vault, even in a `0600` file, collapses the separation between ciphertext and
key material. Silently creating a new key when encrypted data already exists
would also make enrolled profiles permanently unreadable.

A libsecret C binding would add cgo and a `libsecret-1-dev` build dependency to
ordinary Go tests and builds. Invoking `secret-tool` would add subprocess,
output-parsing, cancellation, and deployment ambiguity. The standardized
Secret Service D-Bus API is available independently of either development
surface and supports context-aware calls through the pure-Go godbus client.

## Decision

The production storage adapter uses `github.com/godbus/dbus/v5` to communicate
directly with the login-session Secret Service.

- Open only a `dh-ietf1024-sha256-aes128-cbc-pkcs7` session. Never negotiate
  or fall back to the `plain` session algorithm.
- Search and create with fixed, non-PII attributes: application, purpose,
  schema, and version. Never put a profile, subject, username, host, device,
  model, or filesystem path in lookup attributes.
- Do not request unlock, run a prompt, or wait for prompt completion. A locked
  item or collection, or an operation requiring a prompt, fails closed with a
  typed permission fault. An unavailable service remains a typed availability
  fault and never triggers another storage mechanism.
- Load on every operation and keep no process-wide key cache. The key is never
  written to the vault, runtime directory, log, audit, domain, or application
  layers.
- Serialize lookup-and-create with a private cross-process advisory lock. The
  lock file contains no key material.
- If the key is absent while a finalized encrypted catalog or canonical
  template `.bio` file exists, reject the operation. An empty root, lock file,
  or orphan temporary file does not count as protected data and permits first
  creation.
- After creation, search and decrypt the item again and require the unique
  value to equal the generated nonzero 32-byte key before returning it.
- Unit tests use an injected static fake Secret Service and deterministic
  entropy. They do not connect to or mutate a developer's login keyring.

The D-Bus implementation follows the freedesktop.org
[Secret Service specification](https://specifications.freedesktop.org/secret-service/latest-single/),
including its DH/HKDF/AES transfer protocol. It is pure Go, so normal
`go test ./...` and `make check` do not require libsecret headers, cgo, or an
optional production build tag.

## Consequences

- Ubuntu production requires a working, unlocked login Secret Service. A
  locked or unavailable service is visible to composition and UI as a typed
  failure; there is no silent key regeneration or file fallback.
- Cooperating processes must use the configured runtime lock directory. A
  contender receives an availability fault and may retry explicitly.
- D-Bus transport still crosses desktop processes; the encrypted DH session
  minimizes plaintext exposure but is not an authorization mechanism or a
  defense against a compromised user session.
- Desktop composition of this provider is a separate vertical slice. Static
  keys remain test-only.

## Rejected Alternatives

- libsecret C API/cgo: correct service semantics, but unnecessarily makes the
  build depend on native headers and toolchain configuration.
- `secret-tool` subprocess: weak typed error handling and cancellation, plus
  a runtime executable dependency.
- A `0600` key file or automatic fallback: places key and ciphertext under the
  same filesystem trust boundary and can conceal Secret Service failures.
- Silent regeneration after key loss: irreversibly orphans protected biometric
  data.
