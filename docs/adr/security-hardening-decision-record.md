# Security Hardening — BookOrbit URL scheme/loopback validation & cleartext-credential transport guard — Decision Record

**Status:** Implemented exactly per the approved design
(`docs/security-hardening-bookorbit-url-validation-design.md`). Scope was the
additive security-validation touch-points named there — `internal/util/loopback.go`
(new), `internal/util/url.go`, `internal/util/util_test.go`,
`internal/config/{types,env,load,validate,config_test}.go`, `cmd/bridge/main.go`,
`cmd/bridge/main_test.go`, and four documentation touch-ups
(`configs/bridge.example.yaml`, `docker-compose.yml`, `README.md`,
`systemd/bridge.service`). No package boundary changed and no sync-engine,
BookOrbit-wire-protocol, or Readest-auth-lifecycle behavior was altered — the
change only constrains and warns about operator-supplied configuration, exactly
as the design's §2 goals and §9 summary describe. `go build ./...`,
`go vet ./...`, `gofmt -l .`, `go test ./...`, and `go test -race ./...` all
pass, including every newly added test.

The security hardening is complete. The two compounding problems the design
fixes (cleartext-egress of a password-equivalent credential, and silent
acceptance of any operator-supplied URL string) are now loud at config load
instead of silent at request time.

---

## Decisions implemented (A–J, per the approved design §3)

| # | Decision | Status |
|---|----------|--------|
| A | Where does the check live? | **Unchanged** — new `Validate` clauses appended to the existing `problems []string` slice in `internal/config/validate.go`, matching the aggregate-then-report-once convention since Phase 0. Two private helpers (`appendBookOrbitURLProblems`, `appendReadestURLProblem`) keep the file readable without changing the validation surface. |
| B | Fatal at config load vs warning | **Implemented** — unparseable URL, missing scheme, non-`http(s)` scheme, and `http://` non-loopback without the opt-in boolean are all fatal at load (appended to the `*ValidationError.Problems` slice). `http://` non-loopback *with* the opt-in boolean is warned once at startup (Decision I). |
| C | What counts as "loopback / link-local"? | **Implemented** — literal string match (`localhost`, `localhost.`) or `net.ParseIP` membership in `127.0.0.0/8`, `169.254.0.0/16`, `::1/128`, `fe80::/10`. No DNS resolution at validation time; `nas.local` that happens to resolve to `127.0.0.1` is not auto-accepted. |
| D | Opt-out shape | **Implemented** — one boolean, `bookorbit.allow_insecure_transport` / `BRIDGE_BOOKORBIT_ALLOW_INSECURE_TRANSPORT`, default `false`. It relaxes exactly the `http://` non-loopback check and nothing else; unparseable URLs and bad schemes remain fatal even with the opt-in set. |
| E | Should the Readest URLs get the same treatment? | **Implemented** — `readest.supabase_url` and `readest.sync_base_url` are required to be `https` outright, with no opt-out. The defaults in `internal/config/defaults.go` are already `https://`, so this is a pure misconfiguration guard. |
| F | Does `NormalizeBookOrbitURL` change? | **Implemented** — rewritten to parse with `net/url.Parse` first, reject userinfo / fragment / missing scheme (returning `""` — the sentinel `Validate` already reacts to), then apply the existing three string-suffix rules verbatim. The existing `TestNormalizeBookOrbitURL` cases all still pass unchanged. |
| G | Where do the loopback check helpers live? | **Implemented** — new file `internal/util/loopback.go` with `SchemeOf`, `IsLoopbackOrLinkLocal`, and a tiny unexported-ish `HostOf` (used by `cmd/bridge`'s startup warning). Imports only `net`, `net/url`, `errors`, `strings` — no `internal/*` imports — so the layering stays acyclic (the design's `internal/config` → `internal/util` DAG is preserved). |
| H | Does the new boolean get a `setBool` env helper? | **Implemented** — reuses the Phase 9 `setBool` helper unchanged; new env constant `EnvBookOrbitAllowInsecureTransport = "BRIDGE_BOOKORBIT_ALLOW_INSECURE_TRANSPORT"` added alphabetically near the other BookOrbit env constants; one new line in `applyEnv`. |
| I | Logging the warning — where, and what does it say? | **Implemented** — `cmd/bridge/main.go` emits one `WARN` immediately after the logger is built and before `boClient.Auth(ctx)` runs, gating on `cfg.BookOrbit.AllowInsecureTransport == true` and naming both `url` and `host`. Fires once at startup, never per request. |
| J | Test surface | **Implemented** — `TestSchemeOf`, `TestIsLoopbackOrLinkLocal`, `TestHostOf`, `TestNormalizeBookOrbitURLRejectsInvalid` in `internal/util/util_test.go`; `TestValidateRejectsBadScheme`, `TestValidateRejectsCleartextNonLoopback`, `TestValidateAcceptsLoopbackCleartext`, `TestValidateRejectsReadestHttp`, `TestLoadEnvAllowInsecureTransport`, `TestLoadYamlAllowInsecureTransport`, `TestLoadYamlAllowInsecureTransportInvalidValueRejected` in `internal/config/config_test.go`. |

No decision was reopened or reconsidered. Every item in the design's §3 table
was implemented as specified.

---

## What changed, file by file

### `internal/util/loopback.go` (new)

Three exported functions and one private package-level builder:

- `SchemeOf(rawURL string) (string, error)` wraps `net/url.Parse`, returns the
  lowercase scheme, or an error if `rawURL` is unparseable or has no scheme.
  Pure helper; the policy ("is `http` allowed here?") lives in `config.Validate`.
- `IsLoopbackOrLinkLocal(host string) bool` — strips brackets (`[::1]` → `::1`)
  and a `host:port` tail when present *and* the part after the `:` is all digits
  (the port-stripping rule is restricted to non-bracketed hosts with exactly one
  colon so a bare IPv6 like `::1` is never over-stripped — see "Deviations found
  in testing" below for the rationale); matches `localhost` and `localhost.` by
  string equality; otherwise attempts `net.ParseIP` and tests membership in the
  four loopback/link-local CIDRs via `*net.IPNet.Contains`.
- `HostOf(rawURL string) string` — returns the `host[:port]` portion of a URL,
  preserving brackets for IPv6 so the result can be passed back to
  `IsLoopbackOrLinkLocal`; on parse failure, returns the input verbatim (defensive:
  `HostOf` is only ever called after a URL has passed `Validate`).
- `loopbackOrLinkLocalNets` — a package-level `[]*net.IPNet` constructed once at
  init via `mustParseCIDRs("127.0.0.0/8", "169.254.0.0/16", "::1/128", "fe80::/10")`
  so each `IsLoopbackOrLinkLocal` call does not rebuild it.

The file imports only `errors`, `net`, `net/url`, `strings` — no `internal/*`
imports — confirming the DAG invariant the design called out.

### `internal/util/url.go` (rewrite `NormalizeBookOrbitURL`)

The body now parses with `net/url.Parse` first, rejects userinfo / fragment /
missing scheme by returning `""` (the sentinel `Validate` already reacts to for
"missing URL"), reassembles via `u.String()`, then applies the existing three
string-suffix rules verbatim (trim trailing `/`, collapse `/api/v1/koreader` →
`/api/v1`, append `/api/v1` if absent). The scheme allowlist itself is *not*
duplicated here — the design (§4.6, §5.5) deliberately keeps the normalizer a
pure normalizer and lets `config.Validate` via `util.SchemeOf` surface "must use
http or https" messages, so the two code paths can't disagree on what a valid
URL is. The pre-existing `TestNormalizeBookOrbitURL` cases all use
`http(s)://host[:port]` forms that parse cleanly, so they continue to pass
unchanged; the new `TestNormalizeBookOrbitURLRejectsInvalid` pins the userinfo,
fragment, and no-scheme rejection.

### `internal/util/util_test.go` (extend)

Net-new tests, no existing test modified:

- `TestSchemeOf` — table over `http://`, `HTTPS://` (lowercased),
  `gopher://`, `""`, `//noscheme`, `"not a url at all"` (bare words parse with
  no scheme — caught), `example.com`.
- `TestIsLoopbackOrLinkLocal` — 22 subtests covering `localhost`,
  `localhost.`, every loopback and link-local format (`127.0.0.1`,
  `127.255.255.255`, `169.254.10.20`, `::1`, `[::1]`, `[::1]:8080`, `fe80::1`,
  `[fe80::1]`, `127.0.0.1:3000`, `[127.0.0.1]`, `[127.0.0.1]:3000`), and
  negative cases (`192.168.1.10`, `192.168.1.10:3000`, `10.0.0.1`, `8.8.8.8`,
  `example.com`, `nas.local`, `""`, `"not an ip or host"`).
- `TestHostOf` — table over IPv4, IPv4-with-port, IPv6-bracketed-with-port,
  localhost-with-port, plus an unparseable-input-returns-input assertion.
- `TestNormalizeBookOrbitURLRejectsInvalid` — pins the userinfo / fragment /
  unparseable / no-scheme / empty rejection.

### `internal/config/types.go` (add field)

`BookOrbitConfig` gains one field after `DeviceID`:

```go
AllowInsecureTransport bool `yaml:"allow_insecure_transport"`
```

with a doc comment naming exactly what it relaxes (`http://` non-loopback
cleartext transport) and what it does *not* relax (URL shape: unparseable,
bad scheme, userinfo, fragment).

### `internal/config/env.go` (add env constant + applyEnv line)

- `EnvBookOrbitAllowInsecureTransport = "BRIDGE_BOOKORBIT_ALLOW_INSECURE_TRANSPORT"`
  added to the `const (...)` block, near `EnvBookOrbitDeviceID`, with a comment
  pointing at the doc comment on `BookOrbitConfig.AllowInsecureTransport`.
- `setBool(&c.BookOrbit.AllowInsecureTransport, EnvBookOrbitAllowInsecureTransport)`
  added in `applyEnv` near the other `BookOrbit` env overlays. `setBool` is the
  Phase 9 helper — reused unchanged.

### `internal/config/load.go` (add yaml `applyValues` arm)

```go
case "bookorbit.allow_insecure_transport":
    b, err := strconv.ParseBool(val)
    if err != nil {
        return fmt.Errorf("bookorbit.allow_insecure_transport: %w", err)
    }
    c.BookOrbit.AllowInsecureTransport = b
```

Modeled on the existing `"bridge.sync_status":` arm. The unknown-key rejection
at the bottom of the switch is unchanged and is the safety rail a typo'd
value like `allow_insecure_transport: ture` falls into (it fails to parse),
not silently misconfigures.

### `internal/config/validate.go` (the core check — three new clauses + two helpers)

Three new clauses appended to the existing `problems []string` in `Validate`:

1. **BookOrbit URL shape and cleartext transport**, gated behind the existing
   `ServerURL == ""` check: when the URL is non-empty, `appendBookOrbitURLProblems`
   derives the scheme via `util.SchemeOf`; on error or non-`http(s)` scheme,
   appends one diagnostic; otherwise, when the scheme is `http`, extracts the host
   via `util.HostOf` and (only if `!IsLoopbackOrLinkLocal(host) &&
   !AllowInsecureTransport`) appends the cleartext-transport diagnostic naming
   the host. The shape clause is always run when present; the cleartext clause
   runs only when the shape check passed (so an unparseable URL produces one
   diagnostic rather than two).
2. **Readest URL `https` requirement**, gated behind each Readest URL's
   existing empty check: `readest.supabase_url` and `readest.sync_base_url`
   each get a `must use https` problem when their scheme is not `https` (no
   opt-out, per Decision E). The loopback case is *also* rejected for Readest
   — there is no "loopback is fine" case for a credential traveling to a
   service the operator does not fully control, even when self-hosted.

Two private helpers (`appendBookOrbitURLProblems`, `appendReadestURLProblem`)
keep the policy logic readable without changing the validation surface (both
return `[]string` so callers compose them into the existing
`problems = append(problems, …)` pattern). The existing `AuthKey()` and
`normalizeServerURL()` are unchanged — `AuthKey` is the call site that
*motivates* the transport guard, not the thing being fixed (exactly as the
design's §4.1 spells out).

A one-line comment on the `bridge.sync_status` Phase 9 invariant was retained
unchanged and the new clauses sit alongside it, alphabetical/logical grouping
unchanged.

### `internal/config/config_test.go` (extend + update test fixtures)

Audited and updated per the design's §7 rule:

- `setRequiredEnv` was changed from
  `EnvBookOrbitServerURL = "http://nas:8080"` to `"https://nas:8080"` — a
  non-resolving test value, since `Validate` performs no DNS, with a comment
  recording why a syntactically valid https URL is required by the new
  cleartext-transport guard.
- `TestLoadFromFileAndNormalize` and `TestSyncStatusFromFile` had their
  `bookorbit.server_url` literals changed from `http://...` to `https://...`,
  and `TestLoadFromFileAndNormalize`'s assertion updated from
  `"http://nas:8080/api/v1"` to `"https://nas:8080/api/v1"`. The normalized
  form is otherwise identical because the normalizer only touches the path
  suffix.
- New tests added at the bottom: `TestValidateRejectsBadScheme`,
  `TestValidateRejectsCleartextNonLoopback` (asserts both the default-fatal
  and the opted-in-warned arms), `TestValidateAcceptsLoopbackCleartext`
  (subtests over IPv4 / IPv6 / DNS-localhost / IPv4-link-local loopback forms,
  asserting only that the cleartext clause does *not* fire), `TestValidateRejectsReadestHttp`
  (asserts both Readest URLs are rejected on `http://`, AND that loopback
  `http://127.0.0.1` is *also* rejected — no opt-out for Readest per Decision E),
  `TestLoadEnvAllowInsecureTransport`, `TestLoadYamlAllowInsecureTransport`,
  `TestLoadYamlAllowInsecureTransportInvalidValueRejected`. A small
  `containsProblem` helper matches on substring across the `*ValidationError.Problems`
  slice.
- New `validConfig()` helper builds a fully-valid Config (with the anon key
  populated, since it skips `finalize()`) so each rejection test can mutate one
  field and assert exactly the clause it exercises.

No existing test was weakened. The rule from the design's §7 ("if any
pre-existing test fails because of the new `Validate` clauses, the fix is to
change the *test's URL* to a loopback or `https` value, never to weaken the new
check") was applied verbatim.

### `cmd/bridge/main.go` (startup warning + import)

- `"github.com/Riffsmith/bookorbit-readest-sync/internal/util"` added to the
  import block (alphabetically before `internal/util/httpclient`).
- A `log.Warn(...)` block placed immediately after the logger is built
  (`log := logger.New(...)`) and before `state.NewFileStore(...)`, gated by
  `if cfg.BookOrbit.AllowInsecureTransport`. The warning names `url` and `host`
  (latter via `util.HostOf`). The single-line contract from Decision I ("fires
  once at startup, not on every request, to keep the log signal-to-noise ratio
  high") is realized: production behavior for `https://` and loopback `http://`
  deployments is unchanged (no warning logged), and the only way the warning
  fires is when the operator has explicitly set the boolean — at which point
  the warning is meaningful, not noise.

No other change in `main.go`'s construction order, control flow, or
dependency wiring. The existing one-time BookOrbit connectivity probe,
`--once`/daemon branching, signal handling, and `state.Save()` defer are
all unchanged.

### `cmd/bridge/main_test.go` (update `TestRunOnceFullWiringSurfacesClassifiedError`)

The pre-existing Phase 7 wiring test used `httptest.NewServer` (a plain
`http://127.0.0.1:...` URL) for the Supabase, Readest sync, and BookOrbit
endpoints. With the new `Validate` clauses, the two Readest URLs are
rejected at load (`readest.supabase_url must use https...`) because Readest
has no cleartext opt-out even for loopback. Per the design's §7 rule, the
fix was to change the test's URL form, not to weaken the new check.

The test was rewritten to:

- Use `httptest.NewTLSServer` instead of `httptest.NewServer`. Its URL is
  `https://127.0.0.1:<port>` — satisfies `Validate` for both Readest
  endpoints AND the BookOrbit endpoint (the latter being loopback https,
  the cleanest case).
- Swap `http.DefaultTransport` to `srv.Client().Transport` for the
  duration of the test only, restored via `t.Cleanup`. `srv.Client()` is
  the SDK-blessed httptest helper that returns a configured `*http.Client`
  whose transport trusts the test server's auto-generated cert; routing
  the production path (which builds `&http.Client{}` with `Transport: nil`,
  falling back to `http.DefaultTransport`) through it lets the real
  `PullBooks` HTTP call succeed at TLS verification and reach the
  400-returning `/sync` handler. `InsecureSkipVerify` is *not* used — the
  test cert is properly verified against the test server's own CA, which
  is exactly the trust relationship httptest's own `srv.Client()`
  establishes. The swap is contained: it lives only inside this test,
  and `t.Cleanup` restores the original `http.DefaultTransport`.

The test still exercises the *exact same* chain Phase 7 specified (config
load → state → device ID → Readest/BookOrbit clients → engine construction
→ startup probe → `RunOnce` → error classification → propagation out of
`run()`), with zero mocking. The switch from a plaintext test server to a
TLS test server is the only difference; the assertions on
`errors.Is(err, readest.ErrBadRequest)` and the stderr "warning" content
are unchanged. This is the kind of test-fixture update the design's §7
explicitly authorized ("change the *test's URL* to a loopback or `https`
value") and is *not* a weakening of the security property — the security
property is in `config.Validate`, which is exercised by the tests in
`internal/config/...`; the cmd/bridge test exercises wiring, not the
validation guard.

No other `cmd/bridge` test was modified. `TestRunVersion`, `TestRunHelp`,
`TestRunUnknownFlagIsNotClassifiedAsHelp`, `TestModeName`,
`TestRunMissingConfigInsufficientEnvFailsValidation`, and the entire
`device_test.go` suite pass unchanged.

### Documentation touch-ups (`configs/bridge.example.yaml`, `docker-compose.yml`, `README.md`, `systemd/bridge.service`)

Edit-only, no structural change:

- `configs/bridge.example.yaml`: the `bookorbit.server_url` comment gained a
  one-sentence note pointing at `https://` and at `allow_insecure_transport`
  for the cleartext case; a new commented `# allow_insecure_transport: false`
  field was inserted after `userkey` and before `device_name`, mirroring the
  "auth → behavior → identity" ordering the section already uses. The env-var
  comment header gained `BRIDGE_BOOKORBIT_ALLOW_INSECURE_TRANSPORT`.
- `docker-compose.yml`: the `BRIDGE_BOOKORBIT_SERVER_URL` placeholder was
  changed from `http://192.168.1.10:3000` to `https://192.168.1.10:3000`, with
  a multi-line comment above it naming the password-equivalent credential
  and pointing at `BRIDGE_BOOKORBIT_ALLOW_INSECURE_TRANSPORT`. A new commented
  `# BRIDGE_BOOKORBIT_ALLOW_INSECURE_TRANSPORT: "true"` placeholder was added
  in the "Optional" block so an operator reading top-to-bottom sees the
  opt-in exists.
- `README.md`: the "Install with systemd" section gained a one-sentence
  note preferring `https://` for `bookorbit.server_url` and pointing at
  `bookorbit.allow_insecure_transport`. The README's credential-handling
  paragraph (about KOReader settings) was *not* changed, per the design's
  §4.12 — it documents which credentials to use, not how they travel, and
  the transport note belongs next to the URL config.
- `systemd/bridge.service`: the header comment's "Secrets" paragraph gained
  a one-sentence transport note. The `[Service]` section is unchanged —
  `ProtectSystem=strict`, `PrivateTmp=true`, `NoNewPrivileges=true` are
  already present and appropriate; `LimitCORE=0` (item #2 in the security
  review) is explicitly out of scope for this fix and tracked separately.

---

## Deviations from the approved design

Two small deviations, both implementation details discovered during testing
that surface a *narrower* behavior than the design's wording specified. Neither
weakens the security property.

### 1. `IsLoopbackOrLinkLocal` port-stripping rule is colon-guarded

The design's §4.7 described the port-stripping step as: "if `host` contains a
`:` and the part after the last `:` parses as a port (all digits), strip it."
Implementation confirmed via `strings.LastIndex(host, ":")` initially, but
the first test run surfaced that bare IPv6 loopback (`::1`) and link-local
(`fe80::1`) addresses were mis-stripped: `LastIndex("::1", ":")` finds the
last `:`, and `"1"` is all digits, so the rule stripped `:1`, leaving `"::"`
which doesn't parse as a valid IPv6 → `IsLoopbackOrLinkLocal` returned false.
The bracketed forms (`[::1]`, `[::1]:8080`) worked correctly because the
bracket-unwrapping branch handled them first.

The fix restricts the port-stripping rule to non-bracketed hosts with
exactly **one** colon (the syntactically unambiguous `host:port` form for
IPv4 or hostname). Two or more colons without brackets is a bare IPv6
address (`::1`, `fe80::1`) whose whole form is the address — never strip
it. This is *narrower* than the design's wording and *more* correct: it
makes the function work on the IPv6 cases the design named in Decision J
(`[::1]`, `fe80::1`), and it preserves the IPv4 `host:port` form. The
behavior on every case listed in the design is unchanged except for the
bare-IPv6 cases, which now correctly return `true` instead of `false`.

Recorded in `internal/util/loopback.go`'s port-stripping branch comment.

### 2. `TestLoadEnvAllowInsecureTransport` uses `"true"` not `"yes"`

The design's §6 Step 8 specified `t.Setenv(EnvBookOrbitAllowInsecureTransport, "yes")`
as the example for the env-override test. `strconv.ParseBool`, which the existing
Phase 9 `setBool` helper uses, does NOT accept `yes`/`no`/`on`/`off` — only
`true`/`false`/`1`/`0`/`t`/`f`/`T`/`F`/`TRUE`/`FALSE`. The test would silently
fail to set the boolean (the `setBool` helper is lenient and ignores unparseable
values, matching the duration-override convention), making the assertion a
false-negative — vacuously green not because the boolean was set but because
`setBool` ignored the bad value, and `Validate` then rejected the cleartext URL.

The test uses `"true"` instead, with a comment recording the divergence and
noting the same pre-existing doc inaccuracy in `configs/bridge.example.yaml`'s
`bridge.sync_status` comment ("Accepts true/false (or 1/0, yes/no, on/off)")
which mentions yes/no/on/off but is handled by `strconv.ParseBool` which rejects
them. That doc inaccuracy is pre-existing Phase 9 work and *explicitly out of
scope* per the user's directive not to change other business logic — fixing the
misleading `sync_status` yaml comment is a one-line touch but unrelated to the
security hardening this ADR documents, so it is left alone.

Neither deviation re-opens any design decision. Both are narrower
implementations consistent with the design's intent.

---

## What was deliberately left unchanged

- **The BookOrbit wire protocol** — `internal/util/md5.go`'s `MD5Hex` and
  `internal/config/validate.go`'s `Config.AuthKey()` continue to derive
  `x-auth-key` as the unsalted MD5 of the password. This is a fixed property
  of the BookOrbit protocol (the code is `//nolint:gosec`-annotated) and is
  not reachable from this bridge. The transport guard is the bridge's
  response to *that protocol's weakness*, not a fix for it.
- **`internal/bookorbit/client.go`'s request construction** — `newRequest`
  still builds `c.baseURL+path` by string concatenation. Post-validation,
  `c.baseURL` is guaranteed to be a validated, normalized `http(s)` URL with
  no userinfo, no query, no fragment, and a host that satisfies the transport
  policy; the `path` arguments are package-constant strings. The concatenation
  is therefore safe; no `CheckRedirect` hardening was added (item #5 in the
  review, deliberately out of scope per the design's §2.2).
- **Readest auth lifecycle**, **sync engine**, **state store** — no file under
  `internal/readest`, `internal/bookorbit`, `internal/sync`, `internal/sync/state`,
  or `internal/token` was touched.
- **`cmd/bridge/main.go`'s order of operations** — the only addition is the
  warning block, after the logger is built and before the state store is
  opened. No construction-order, control-flow, or dependency-wiring change.
- **`internal/config/validate.go`'s `Config.AuthKey()` and
  `normalizeServerURL()`** — these are the call sites the design's §4.1 calls
  out as *motivating* the transport guard, not the things being fixed.
- **`LimitCORE=0` in the systemd unit** (security review item #2) and
  **`CheckRedirect` cross-host hardening** (item #5) — both deliberately
  out of scope per the design's §2.2, tracked separately.

---

## Verification performed

```
$ gofmt -l .                       # no files reported (everything formatted)
$ go vet ./...                     # clean, no diagnostics
$ go build ./...                    # builds clean
$ go test ./...                    # all packages green
$ go test -race ./...              # race-clean
```

Individually run to confirm subtest names and pass status (per the project's
Phase 7/8/9 ADR convention):

- `internal/util`: `TestSchemeOf`, `TestIsLoopbackOrLinkLocal` (22 subtests),
  `TestHostOf`, `TestNormalizeBookOrbitURLRejectsInvalid`, plus the unchanged
  `TestNormalizeBookOrbitURL` — all pass.
- `internal/config`: the seven new tests enumerated under Decision J plus the
  updated `TestLoadFromFileAndNormalize` and `TestSyncStatusFromFile` — all pass.
- `cmd/bridge`: the rewritten `TestRunOnceFullWiringSurfacesClassifiedError`
  plus every unchanged test in `main_test.go` and `device_test.go` — all pass.

### Manual smoke check (§7), reproduced verbatim

From the repo root, with no real credentials and no real network beyond the
attempt (no DNS is done by `Validate`, and the destination IP is unreachable
on a developer machine so no real BookOrbit server is contacted):

**Without the opt-in** — bridge exits non-zero with the cleartext-transport
problem in the `ValidationError` output:

```
$ BRIDGE_READEST_EMAIL=x BRIDGE_READEST_PASSWORD=x \
  BRIDGE_BOOKORBIT_SERVER_URL='http://192.168.1.10:3000' \
  BRIDGE_BOOKORBIT_USERNAME=x BRIDGE_BOOKORBIT_PASSWORD=x \
  go run ./cmd/bridge --once --config /nonexistent
bridge: config: invalid configuration:
  - bookorbit.server_url uses cleartext http to a non-loopback host (192.168.1.10:3000); the x-auth-key header is the MD5 of your password (a password-equivalent credential). Use https://, or set bookorbit.allow_insecure_transport: true to acknowledge the cleartext risk.
$ echo $?
1
```

**With the opt-in** — bridge proceeds past `Validate`, logs the cleartext
`WARN` naming `url` and `host`, logs `bridge starting`, then fails on the
(unreachable) BookOrbit connectivity probe as a classified `ErrNetwork`
rather than as an auth error — the URL passed validation, and the cleartext
egress is documented in the daemon log, not silent:

```
$ BRIDGE_READEST_EMAIL=x BRIDGE_READEST_PASSWORD=x \
  BRIDGE_BOOKORBIT_SERVER_URL='http://192.168.1.10:3000' \
  BRIDGE_BOOKORBIT_USERNAME=x BRIDGE_BOOKORBIT_PASSWORD=x \
  BRIDGE_BOOKORBIT_ALLOW_INSECURE_TRANSPORT=true \
  go run ./cmd/bridge --once --config /nonexistent
bridge: warning: config: file not found: /nonexistent; using defaults and environment
{"time":"...","level":"WARN","msg":"bridge: bookorbit.server_url uses cleartext http to a non-loopback host; x-auth-key (MD5 of password) will travel in cleartext","url":"http://192.168.1.10:3000/api/v1","host":"192.168.1.10:3000"}
{"time":"...","level":"INFO","msg":"bridge starting","version":"0.1.0-dev","mode":"once","state_file":"bridge-state.json","device_id":"...","poll_interval":"15m0s"}
{"time":"...","level":"WARN","msg":"bookorbit connectivity check failed; sync will retry on schedule","error":"bookorbit: network error: GET /api/v1/koreader/users/auth: Get \"http://192.168.1.10:3000/api/v1/koreader/users/auth\": dial tcp 192.168.1.10:3000: connect: connection refused"}
$ echo $?
0
```

The second invocation exits 0 only because `--once`'s `runErr` is an
`ErrNetwork` that the engine's own retry/backoff already exhausted; the
daemon-mode path would also `Restart=on-failure` only on a genuine
infrastructure-level exit (the URL is validated; credentials have not
leaked; the operator has a single startup WARN documenting their decision).
The default `https://` deployment logs nothing extra.

A future maintainer reproducing this can run the two commands above; the
exact YAML/env shape is documented in `configs/bridge.example.yaml` and
`docker-compose.yml`.

---

## Out-of-scope items this implementation deliberately does not address

Listed so a future maintainer does not accidentally try to fix them as part
of this work — exactly mirroring the design's §8, included here verbatim for
traceability:

- **#2** Memory-resident Readest password + core dumps (`LimitCORE=0` in the
  systemd unit, container seccomp). Small but touches operational hardening.
- **#4** `BRIDGE_SUPABASE_ANON_KEY` base64 auto-decode heuristic. The anon
  key is public; the practical impact is debugging-confusion, not security.
- **#5** `http.Client` redirect-following. Go already strips `Authorization`,
  `x-auth-user`, `x-auth-key` on cross-host redirects (since Go 1.8), so the
  practical impact is small; ship independently.
- **#6 / #7** Compose secrets hygiene and container runtime hardening. Worth
  a dedicated "deployment hardening" change.
- **#8** `govulncheck` in CI. Unrelated to the security properties of the
  bridge's own code; a CI-pipeline change.

---

*The design document (`docs/security-hardening-bookorbit-url-validation-design.md`)
is left as-is, per the project's established convention (design docs are
historical records; this ADR is where the as-built account lives — the same
pattern every prior phase's ADR followed relative to its design doc).*
