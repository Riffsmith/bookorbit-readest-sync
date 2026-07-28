// Package util holds small, pure helper functions that several packages rely on.
//
// These helpers are ports of behaviors that the reference KOReader plugins
// implement in Lua, re-expressed idiomatically in Go. They are deliberately
// free of I/O and third-party dependencies so they can be unit-tested in
// isolation and reused by the Readest and BookOrbit clients and the sync
// engine without pulling in heavier machinery.
//
// Nothing in this package performs network access; it only transforms values.
package util
