Goal

Standalone daemon.

Readest
↓

Readest Sync

↓

Standalone Bridge

↓

BookOrbit

Language

Go

Architecture

Polling daemon

Readest is the only source of truth.

BookOrbit is write-only.

No KOReader runtime.

No UI.

No annotations.

No highlights.

No status sync.

No bidirectional sync.

Design constraints

Never copy plugin code.

Plugins are reference implementations only.

Implementation should be idiomatic Go.

Testing

Every module should have unit tests.

No TODOs in production code.

Stop after every milestone.
