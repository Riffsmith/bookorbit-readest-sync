// Package readest contains the Readest side of the bridge: authentication
// against the Supabase backend and the library sync client.
//
// The package defines the shared data model (the `/sync?type=books` row
// shape) and the interfaces the sync engine consumes, plus the concrete
// Supabase auth flow (Phase 3) and the HTTP sync client that pulls changed
// book rows since a watermark (Phase 4).
//
// This package knows nothing about BookOrbit; only internal/sync imports both.
package readest
