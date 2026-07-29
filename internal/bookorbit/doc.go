// Package bookorbit contains the BookOrbit side of the bridge: the request and
// response models for the kosync-compatible REST surface, and the HTTP client
// that calls it.
//
// The client is a thin, faithful transport: it turns domain-level sync facts
// (hashes + candidate metadata, progress items) into BookOrbit HTTP calls, and
// BookOrbit's JSON responses back into typed Go values or classified sentinel
// errors. Authentication is static (x-auth-user/x-auth-key), with no token
// lifecycle to manage. Batching, retries, the match/unmatched cache, and any
// fallback policy between the bulk and singular progress endpoints are the
// sync engine's responsibility (internal/sync), not this package's.
//
// This package knows nothing about Readest; only internal/sync imports both.
package bookorbit
