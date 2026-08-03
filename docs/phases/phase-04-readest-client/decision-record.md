# Phase 4 — Decision Record

**Status:** Implemented. Phase 4 (Readest sync client) is complete and verified.

## What changed

 - internal/readest/client.go — sync client (pull/push against /sync/books, /sync/progress) with auth-retry on 401/403, watermark handling, and the body-shape detection from the plugin
  analysis.
  - internal/readest/client_test.go — 69 test cases covering request shape, headers, watermark/since handling, 401→reauth→retry, malformed responses, and dummy/deleted row filtering.
  - Fixed the fakeAuth struct formatting; full suite passes under -race (84.8% coverage on the readest package).

