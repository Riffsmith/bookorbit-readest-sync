// Package readest contains the Readest side of the bridge: authentication
// against the Supabase backend and the library sync client.
//
// In this foundation phase the package defines the shared data model (the
// `/sync?type=books` row shape) and the interfaces the sync engine will
// consume. The concrete Supabase auth flow and HTTP sync client are stubs that
// return ErrNotImplemented; they are filled in during Phases 3 and 4 of the
// roadmap. Keeping the types and interfaces here now lets the engine and tests
// be written against stable contracts before any network behavior exists.
//
// This package knows nothing about BookOrbit; only internal/sync imports both.
package readest
