// Package bookorbit contains the BookOrbit side of the bridge: the request and
// response models for the kosync-compatible REST surface and the client that
// will call it.
//
// In this foundation phase the package defines the payload shapes (match-check,
// bulk progress) and the interface the sync engine will use. The concrete HTTP
// client is a stub returning ErrNotImplemented; it is implemented in Phase 5
// of the roadmap. Defining the shapes now locks the contract the engine and
// tests build against.
//
// This package knows nothing about Readest; only internal/sync imports both.
package bookorbit
