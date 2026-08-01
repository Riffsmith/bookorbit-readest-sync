# Security Hardening — BookOrbit URL scheme/loopback validation & cleartext-credential transport guard

**Status:** design proposed, not yet implemented. This document is the
authoritative, hand-held guide an implementer follows end to end. It records
the findings, the exact decisions, and a file-by-file implementation plan
(including test cases and the verification commands to run). No business logic
is changed by this document; the changes described here constrain and warn
about operator-supplied configuration, they do not alter the sync engine,
the BookOrbit wire protocol (MD5 `x-auth-key`), or the Readest auth lifecycle.

---

## 1. Problem statement (the two issues this fixes)

These are items #3 and #1 from the security review, fixed together because
they compound: #1 makes cleartext transport *catastrophic*, and #3 is what
makes cleartext transport *reachable by accident*.

### 1.1 #1 — BookOrbit `x-auth-key` is an unsalted MD5 of the password

`internal/util/md5.go:14` (`MD5Hex`) and `internal/config/validate.go:99`
(`Config.AuthKey`) derive the `x-auth-key` header as the lowercase hex MD5 of
the account password. This is a fixed property of the BookOrbit wire
protocol (the code is correctly `//nolint:gosec`-annotated), so it is **not
fixable in this bridge**. The consequence that *is* fixable here:

> The MD5 of the password is a *password-equivalent credential*. Anyone who
> observes it on the wire can impersonate the user to BookOrbit, and because
> it is an unsalted hash of the bare password, it is also crackable offline
> (rainbow tables / GPU brute force) for any non-trivial password. Therefore
> the BookOrbit server URL **must** be `https://`, **or** `http://` to a
> loopback / link-local address where the operator has positively asserted
> the transport is private. Anything else broadcasts a
> password-equivalent credential in cleartext.

The bridge today does nothing to enforce or even warn about this. The README
and `docker-compose.yml` actively point a new user at
`http://192.168.1.10:3000` — a plaintext LAN URL — with no caveat.

### 1.2 #3 — `bookorbit.server_url` is validated only for non-emptiness

`internal/config/validate.go:38` rejects only an empty `ServerURL`. Nothing
checks the scheme, the host, or the shape. Combined with
`internal/util/url.go:NormalizeBookOrbitURL` (which does pure
string-prefix manipulation and never parses the URL), an operator — or an
attacker who can set `BRIDGE_BOOKORBIT_SERVER_URL` — can supply any string
and the bridge will dutifully send `x-auth-user` / `x-auth-key` to it on
every request:

- `https://evil.example.com/api/v1` → silent credential exfiltration to an
  attacker host.
- `http://anything/api/v1` → cleartext broadcast of the password-equivalent
  credential (the #1 problem, made trivial by the absence of any scheme
  check).
- `gopher://…/api/v1`, `file:///…/api/v1`, `javascript:…` → the standard
  library's `http.NewRequest` will reject most of these at request build
  time, but the failure surfaces as an opaque "build request" error deep in
  `internal/bookorbit/client.go`, not at config load where it belongs.

The threat model is **not** "the operator is the adversary" — it's "the
operator copy-pastes a URL from the example compose file, or an attacker who
can write one environment variable, and the bridge gives no signal that
credentials are about to leave the box in cleartext or to an unexpected
host." The fix is to make the unsafe cases *loud* at config load, where the
operator still has a chance to correct them, instead of silently sending
credentials.

---

## 2. Goals and non-goals

### 2.1 Goals

1. Reject `bookorbit.server_url` that is not parseable as a valid `http(s)`
   URL at config load, with a clear `ValidationError` message identifying
   the problem (no scheme, bad scheme, unparseable).
2. Reject `bookorbit.server_url` whose scheme is `http://` *and* whose host
   is not a loopback or link-local address, **unless** the operator has
   explicitly opted into cleartext transport for that host.
3. Provide the opt-out: a single new boolean config field
   `bookorbit.allow_insecure_transport` (yaml) /
   `BRIDGE_BOOKORBIT_ALLOW_INSECURE_TRANSPORT` (env), default `false`. When
   `true`, an `http://` non-loopback URL is accepted and a `WARN` is logged
   at startup naming the host and the credential that will travel in
   cleartext. The opt-out is deliberately permissive (one boolean, not a
   host list) to keep the surface small and the failure mode obvious.
4. Apply the same scheme/loopback check to `readest.supabase_url` and
   `readest.sync_base_url`, but with `https` required outright (no opt-out):
   these are credentials (Supabase password grant, Bearer token) traveling
   to a third-party service, and there is no sane reason to send them over
   cleartext `http://`. The hosted defaults in
   `internal/config/defaults.go` (`https://readest.supabase.co`,
   `https://web.readest.com/api`) already satisfy this; the check protects
   a self-hosted-Readest operator from a misconfiguration.
5. Move the URL parsing concern *out of* `NormalizeBookOrbitURL`'s
   string-prefix manipulation and into a real `net/url.Parse` so that
   `user:pass@host` userinfo, query strings, fragments, and emoji-IDN
   hosts are surfaced as validation problems rather than silently
   concatenated into a request URL.
6. Update `configs/bridge.example.yaml`, `docker-compose.yml`, `README.md`,
   and `systemd/bridge.service`'s header comment so that the documented
   examples and the placeholder defaults no longer steer a new user at a
   plaintext LAN URL without comment.

### 2.2 Non-goals

- **Not** replacing MD5 in the BookOrbit protocol — that is upstream's call
  and not reachable from this bridge.
- **Not** adding a TLS pinning / certificate-CA allowlist for BookOrbit —
  Go's default system verification is the right baseline; pinning would
  break operators who rotate BookOrbit's TLS cert.
- **Not** zeroizing the Readest password in memory (Go strings are
  immutable; that is a separate, larger change and is item #2 in the review,
  not part of this fix).
- **Not** adding `CheckRedirect` hardening (item #5 in the review) — that
  is a separate, smaller change and is not coupled to #1+#3. It can ship
  independently.
- **Not** changing the BookOrbit client's request construction
  (`internal/bookorbit/client.go:newRequest`, `c.baseURL+path`). The
  validation happens at config load; once a URL has passed validation and
  normalization, the existing string concatenation is safe because the
  `path` arguments are package-constant strings (`"/koreader/users/auth"`,
  etc.) and the validated `baseURL` carries no userinfo, query, or fragment.

---

## 3. Settled decisions

| #   | Decision                                                            | Answer (as to be implemented)                                                                                                                                                                                                                                                                                                                                                                      |
| --- | ------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| A   | Where does the check live?                                          | In `internal/config/validate.go`, as new `Validate` clauses that append to the existing `problems []string` slice. `Validate` already aggregates all problems and reports them at once (the project convention since Phase 0); a hardening check that fits that shape belongs there, not in a new validator.                                                                                       |
| B   | Is the check fatal (hard error) or warning?                        | **Fatal at config load** for: unparseable URL, missing scheme, non-`http(s)` scheme, and `http://` non-loopback without the opt-in boolean. **Warned at startup** for: `http://` non-loopback *with* the opt-in boolean. The fatal-at-load policy matches how `Validate` already treats every other "the bridge cannot operate safely with this" condition (empty credentials, non-positive intervals). |
| C   | What counts as "loopback / link-local"?                            | Host resolves to a loopback IPv4/6 (`127.0.0.0/8`, `::1/128`) **or** a link-local IPv4/6 (`169.254.0.0/16`, `fe80::/10`). The check is on the *literal host string*, not on a live DNS resolution: a hostname like `localhost` is accepted by string match; an A-record host like `nas.local` is **not** auto-accepted even if it happens to resolve to 127.0.0.1, because doing a DNS lookup at config load is a side effect `Validate` does not otherwise have and would make validation order-dependent. The operator who wants `nas.local` over plaintext must set the opt-in boolean. |
| D   | Opt-out shape                                                       | One boolean, `bookorbit.allow_insecure_transport`, default `false`. Not a host allowlist, not a "skip all transport checks" escape hatch — it relaxes exactly the `http://`-non-loopback check and nothing else. Unparseable URLs and bad schemes are still fatal even with the opt-in set.                                                                                                          |
| E   | Should the Readest URLs get the same treatment?                    | Yes, but stricter: `https` required for both `readest.supabase_url` and `readest.sync_base_url`, no opt-out. These carry a Supabase password grant and a Bearer token to a third-party service; cleartext is never acceptable. The defaults are already `https://`, so this is a pure misconfiguration guard.                                                                                         |
| F   | Does `NormalizeBookOrbitURL` change?                               | Yes — it is rewritten to parse with `net/url.Parse` first, validate, and only then do the existing string-suffix normalization (strip trailing `/`, collapse `/api/v1/koreader` → `/api/v1`, append `/api/v1` if absent). The string-suffix rules are preserved verbatim so the existing `TestNormalizeBookOrbitURL` cases all still pass; the new parse step runs before them and rejects inputs the old code silently accepted. |
| G   | Where does the loopback check helpers live?                        | In `internal/util`, alongside `url.go`. A new file `internal/util/loopback.go` with two pure functions: `IsLoopbackOrLinkLocal(host string) bool` and `SchemeOf(rawURL string) (string, error)` (returns the lowercase scheme, or an error if the URL is unparseable or has no scheme). `internal/util` already imports nothing from `internal/config`, so the layering stays acyclic.         |
| H   | Does the new boolean get a `setBool` env helper?                  | Yes — `internal/config/env.go` already has `setBool` (added in Phase 9 for `bridge.sync_status`); reuse it unchanged. Add a new env constant `EnvBookOrbitAllowInsecureTransport = "BRIDGE_BOOKORBIT_ALLOW_INSECURE_TRANSPORT"`.                                                                                                                                                                       |
| I   | Logging the warning — where, and what does it say?                 | In `cmd/bridge/main.go`, immediately after `config.Load` succeeds and the logger is built, before `boClient.Auth(ctx)` runs. The warning is `log.Warn("bridge: bookorbit.server_url uses cleartext http to a non-loopback host; the x-auth-key header (an MD5 of your password) will travel in cleartext", "url", cfg.BookOrbit.ServerURL, "host", hostOf(cfg.BookOrbit.ServerURL))`. The warning fires once at startup, not on every request, to keep the log signal-to-noise ratio high. |
| J   | Test surface                                                        | New unit tests in `internal/util/util_test.go` (loopback/scheme helpers, including the cases the review's threat model implies: `localhost`, `127.0.0.1`, `::1`, `169.254.x.x`, `fe80::`, `[::1]`, `192.168.1.10`, `evil.example.com`, `http://`, `https://`, `gopher://`, empty, `"//noscheme"`). New unit tests in `internal/config/config_test.go` for the new `Validate` clauses and the env override. The existing `TestNormalizeBookOrbitURL` cases continue to pass unchanged. |

---

## 4. Findings — what the relevant files currently do

Read these notes alongside the design; they are the facts the implementation
plan in §6 is built on. Each item cites the file and line an implementer will
touch.

### 4.1 `internal/config/validate.go`

- `Validate()` (line 16) builds a `problems []string` slice and returns a
  `*ValidationError{Problems: problems}` (lines 73–90) when non-empty. Every
  existing check (lines 20–66) appends a single human-readable string. New
  checks follow the same pattern: append to `problems`, return the
  aggregated error. **There is no early return** — the operator sees every
  problem at once, which is the project convention since Phase 0.
- `Config.AuthKey()` (line 95) is where the MD5 of the password is computed;
  this is the credential that travels as `x-auth-key`. This function is
  **not changing** — it is the call site that motivates the transport guard,
  not the thing being fixed.
- `normalizeServerURL` (line 9) is the one-line delegation to
  `util.NormalizeBookOrbitURL`; called from `finalize()` (`load.go:71`).

### 4.2 `internal/config/load.go`

- `Load` (line 24) calls `cfg.applyEnv()` (line 52), `cfg.finalize()` (line
  53), then `cfg.Validate()` (line 55). The new check runs inside
  `Validate`, after `finalize()` has already normalized the URL — so the
  check sees the post-normalization form (`<scheme>://<host>[:port]/api/v1`).
  **Ordering matters**: the new validation must run against the normalized
  URL, not the raw operator input, because normalization can append `/api/v1`
  and that must not trip the scheme check. Implementer confirms this by
  reading `load.go:63-72` and verifying the scheme is preserved by
  `NormalizeBookOrbitURL` (it is — the function never touches the scheme).

### 4.3 `internal/config/env.go`

- `setBool` (line 81) is already present from Phase 9; reuse it. New env
  constant goes in the `const (...)` block at lines 13–32, alphabetically
  near `EnvBookOrbitDeviceID` / `EnvBookOrbitPassword`. The naming
  convention is `BRIDGE_<SECTION>_<FIELD>` so the new constant is
  `EnvBookOrbitAllowInsecureTransport = "BRIDGE_BOOKORBIT_ALLOW_INSECURE_TRANSPORT"`.
- The `applyEnv` body (line 37) gains one line: `setBool(&c.BookOrbit.AllowInsecureTransport, EnvBookOrbitAllowInsecureTransport)`.

### 4.4 `internal/config/types.go`

- `BookOrbitConfig` (line 35) gains one field:
  `AllowInsecureTransport bool yaml:"allow_insecure_transport"`. Slot it
  after `DeviceID` (line 51) to keep fields roughly in "required →
  identity → behavior" order. The doc comment must call out that this
  relaxes *only* the `http://`-non-loopback check, not the URL-shape checks.

### 4.5 `internal/config/yaml.go` / `internal/config/load.go:applyValues`

- `applyValues` (line 77) is a `switch` over dotted keys with a `default`
  that rejects unknown keys (line 174). Add a new `case
  "bookorbit.allow_insecure_transport":` arm that parses the value with
  `strconv.ParseBool` (mirroring the `bridge.sync_status` arm at lines
  143–148) and assigns to `c.BookOrbit.AllowInsecureTransport`. **The
  unknown-key rejection is the safety rail** — a typo'd
  `allow_insecure_transport: ture` fails to parse, not silently misconfigures.

### 4.6 `internal/util/url.go`

- `NormalizeBookOrbitURL` (line 17) currently does pure string
  manipulation. It must be rewritten to:
  1. `url.Parse(u)` — if it errors, return `""` (the empty return is
     already the sentinel `validate.go` reacts to for "missing URL", and a
     malformed URL is conceptually the same failure: the bridge cannot
     operate with this value). The implementer must verify that an empty
     return still produces the existing error message in `Validate` (it
     does — `validate.go:38` checks `== ""`).
  2. Reject userinfo (`u.User != nil`) by returning `""` — credentials
     baked into the URL are a footgun the BookOrbit protocol does not use
     (auth is via `x-auth-user`/`x-auth-key` headers), and a URL like
     `http://user:pass@nas/api/v1` would today be silently passed through and
     the userinfo discarded by Go's HTTP client, masking the operator's
     mistaken belief that they configured auth.
  3. Reject a non-empty fragment (`u.Fragment != ""`) by returning `""` —
     a fragment on an API base URL is meaningless and indicates a paste
     error.
  4. Keep the existing string-suffix rules verbatim: trim trailing `/`,
     collapse `/api/v1/koreader` → `/api/v1`, append `/api/v1` if absent.
     These operate on the parsed-and-reassembled URL string, so the
     existing `TestNormalizeBookOrbitURL` cases pass unchanged.
- **Why not move the scheme/loopback check into `NormalizeBookOrbitURL`?**
  Because `NormalizeBookOrbitURL` is a pure normalizer (it has callers in
  tests that expect it to normalize without rejecting), and the
  loopback/opt-in logic needs `Config.AllowInsecureTransport`, which the
  `util` package cannot import (`internal/config` imports `internal/util`,
  not the reverse — see the layering note in `internal/util/url.go`'s
  package comment and the import graph in `internal/config/validate.go:4`).
  The split is: `util` does parse + scheme extraction + loopback test (pure
  helpers, no config dependency); `config.Validate` does the policy (fatal
  vs warn, opt-in boolean). This is the same seam the project has used
  since Phase 4.

### 4.7 `internal/util/loopback.go` (new file)

- `SchemeOf(rawURL string) (scheme string, err error)` — wraps `url.Parse`,
  returns the lowercase scheme, or an error if the URL is unparseable or
  has an empty scheme. Keep the function trivial; the policy decision
  ("is `http` allowed here?") lives in `config.Validate`.
- `IsLoopbackOrLinkLocal(host string) bool` — strips a `[...]` IPv6 bracket
  pair and a port (`host:port` → `host`), then matches against the literal
  strings `localhost` and `localhost.` (the trailing-dot form, which some
  resolvers accept), and against `net.IP` after attempting `net.ParseIP`. If
  `net.ParseIP` succeeds, the IP is tested against the loopback and
  link-local CIDRs using the standard library's `net.IPNet` covers idiom
  (an `ipnet.Contains(ip)` check against the four documented ranges from
  Decision C). If `net.ParseIP` fails (e.g., the host is a DNS name other
  than `localhost`), return `false` — per Decision C, no DNS resolution at
  validation time.
- Place these in `internal/util` so `internal/config` (and only
  `internal/config`) imports them. No other package needs them; the
  BookOrbit client and Readest client consume the already-validated URL
  string.

### 4.8 `internal/bookorbit/client.go` — NO change

- `newRequest` (line 425) builds `c.baseURL+path` by string concatenation.
  After this fix, `c.baseURL` is guaranteed to be a validated, normalized
  `http(s)` URL with no userinfo, no query, no fragment, and a host that
  satisfies the transport policy. The `path` arguments are package-constant
  strings. The concatenation is therefore safe post-validation. The
  implementer confirms by reading `cmd/bridge/main.go:104` (where
  `NewClient` receives `cfg.BookOrbit.ServerURL`, already normalized) that
  no un-validated URL reaches the client.

### 4.9 `cmd/bridge/main.go`

- The cleartext warning (Decision I) goes after the logger is built (line 83)
  and before `boClient.Auth(ctx)` (line 134). Use a small helper
  `warnInsecureTransport(cfg, log)` (define it in `main.go` near
  `modeName`) so the condition reads cleanly:
  ```go
  if cfg.BookOrbit.AllowInsecureTransport {
      host := util.HostOf(cfg.BookOrbit.ServerURL) // new tiny helper, same file as SchemeOf
      log.Warn("bridge: bookorbit.server_url uses cleartext http to a non-loopback host; x-auth-key (MD5 of password) will travel in cleartext",
          "url", cfg.BookOrbit.ServerURL, "host", host)
  }
  ```
  This fires once at startup. It does not fire on every request (the per-
  request path is the same regardless of scheme, so logging it per-request
  would be noise). The `if AllowInsecureTransport` guard means the warning
  is only emitted when the operator has explicitly opted in — i.e., when
  the warning is meaningful, not on every `https://` or loopback `http://`
  deployment.
- Add `util` to the import list (it is not currently imported by
  `cmd/bridge/main.go`; verify with `goimports` / the project's
  `gofmt -s` check before committing).

### 4.10 `configs/bridge.example.yaml`

- The `bookorbit` section (lines 37–53) gains a new commented field:
  ```yaml
  # Allow cleartext http:// to a non-loopback bookorbit.server_url. The
  # x-auth-key header is the MD5 of your password (a password-equivalent
  # credential); https:// is strongly preferred. Set true only if your
  # BookOrbit server has no TLS and you accept the cleartext risk.
  # allow_insecure_transport: false
  ```
  Slot it after `userkey` and before `device_name`, mirroring the
  "auth → behavior → identity" ordering the section already uses.
- The `server_url` comment (line 38–40) gets one appended sentence:
  "Use `https://` whenever possible; see `allow_insecure_transport` below
  for the cleartext case."

### 4.11 `docker-compose.yml`

- The commented placeholder `BRIDGE_BOOKORBIT_SERVER_URL: "http://192.168.1.10:3000"`
  (line 16) is changed to `"https://192.168.1.10:3000"` with the comment
  line directly above it reading: `# Use https:// if your BookOrbit server
  # supports TLS; the x-auth-key header is a password-equivalent. For
  # cleartext http:// to a LAN host, also set
  # BRIDGE_BOOKORBIT_ALLOW_INSECURE_TRANSPORT: "true".`
- A new commented placeholder is added:
  `# BRIDGE_BOOKORBIT_ALLOW_INSECURE_TRANSPORT: "true"`
  so the operator who reads compose top-to-bottom sees the opt-in exists.

### 4.12 `README.md`

- The "Install with systemd" section (around line 87) gains a one-sentence
  note: "Prefer `https://` for `bookorbit.server_url`; the `x-auth-key`
  header is the MD5 of your password. See
  `bookorbit.allow_insecure_transport` in `configs/bridge.example.yaml`
  for the cleartext case."
- The README's credential-handling paragraph (existing lines around 30–40
  about KOReader settings) does **not** change — it documents *which*
  credentials to use, not *how they travel*, and the transport note belongs
  next to the URL config, not the credential lookup.

### 4.13 `systemd/bridge.service`

- The header comment (lines 1–15) gains one line in the "Secrets" paragraph
  (lines 13–15): "Prefer `https://` for `bookorbit.server_url`; the
  `x-auth-key` header is the MD5 of the password (a
  password-equivalent). Use `bookorbit.allow_insecure_transport` only if
  your BookOrbit server has no TLS."
- The `[Service]` section is unchanged — `ProtectSystem=strict`,
  `PrivateTmp=true`, `NoNewPrivileges=true` are already present and
  appropriate. Adding `LimitCORE=0` (to prevent the Readest password from
  landing in a core dump, item #2 in the review) is **out of scope** for
  this fix and tracked separately.

---

## 5. Decision record — why these choices

### 5.1 Why fatal-at-load, not runtime warning

The project's `Validate` already treats "the bridge cannot operate safely
with this configuration" as fatal at load (empty credentials, non-positive
intervals, non-MD5 userkey). A cleartext-egress of a password-equivalent
credential is the same class of condition: the bridge *can* technically
operate, but it should not silently do so. Fatal-at-load gives the operator
a clear signal at the moment they can still fix it (config edit + restart),
instead of buried in a daemon log hours later after credentials have
already leaked. The opt-in boolean preserves operator autonomy: an
operator who has positively decided "my LAN is private, my BookOrbit server
has no TLS" can set one flag and proceed, with a startup WARN documenting
the decision.

### 5.2 Why no DNS resolution at validation time

Two reasons. First, `Validate` is currently a pure function of `Config`
with no I/O — every existing check is a string/number comparison. Adding
DNS would make validation order-dependent (the check's outcome would
depend on `/etc/resolv.conf`, network state, and timing) and would make the
tests non-hermetic. Second, a hostname like `bookorbit.internal` resolving
to `127.0.0.1` is not actually a guarantee of loopback transport — the
hostname could resolve differently at request time, or the operator could
be relying on a hosts-file entry that is not present in the validation
environment. The string-only check (`localhost`, `127.x.x.x`, `::1`,
`169.254.x.x`, `fe80::`) is deterministic and conservative: if the host is
not obviously private by its literal form, the operator must opt in. This
matches the principle that a security check should fail safe (default
deny) on ambiguous input.

### 5.3 Why a single boolean opt-out, not a host allowlist

A host allowlist ("`bookorbit.insecure_hosts: [192.168.1.10,
10.0.0.5]`") would be more precise, but it adds a config field type the
project doesn't otherwise have (the custom YAML parser in
`internal/config/yaml.go` rejects sequences/lists — see the package comment
at lines 8–24: "Not supported (and rejected with an error): nested maps
beyond one level, sequences/lists, anchors, multi-line scalars"). Adding
list support to the parser is a much larger change and a much larger
attack surface (the parser is hand-rolled and security-sensitive). The
single boolean keeps the parser unchanged, the config field count minimal,
and the failure mode binary: either cleartext non-loopback is allowed (and
warned) or it is not. An operator who has multiple BookOrbit servers
configured per-bridge is already outside the documented deployment shape
(one bridge → one BookOrbit server, see `README.md` and the docker-compose
single-service layout).

### 5.4 Why `https` is required outright for Readest URLs (no opt-out)

The Readest credentials are a Supabase password grant (sent as JSON to the
Supabase `/auth/v1/token` endpoint) and a Bearer access token (sent as an
`Authorization` header to the Readest sync API). Both travel to
third-party-operated services (the hosted `readest.supabase.co` /
`web.readest.com`, or an operator's own self-hosted Supabase). Sending
either over cleartext `http://` is never acceptable — there is no
"loopback is fine" case for a credential traveling to a service the
operator does not fully control, and the only documented self-hosted
Readest setups use Supabase, which has its own TLS expectations. The
defaults in `internal/config/defaults.go:10-12` are already `https://`,
so the check is a pure misconfiguration guard: it cannot fire on a default
config, only on an explicit override, and the override is wrong.

### 5.5 Why move URL parsing into `NormalizeBookOrbitURL` instead of a new validator

`NormalizeBookOrbitURL` is the single funnel through which every
`bookorbit.server_url` value passes (called from `load.go:71` via
`finalize()`). Putting the parse step there means the URL is *parsed*,
not just *string-manipulated*, before it ever reaches the rest of the
system. The alternative — a new `validateURL` function called from
`Validate` — would duplicate the parse and risk the two code paths
disagreeing on what a valid URL is. The single-funnel approach means the
validated, normalized URL the rest of the code sees is exactly the one that
was checked. The existing test cases for `NormalizeBookOrbitURL`
(`internal/util/util_test.go:TestNormalizeBookOrbitURL`) all use
`http(s)://host[:port]` forms that parse cleanly, so they continue to pass.

---

## 6. Implementation plan — file by file, in order

The order is chosen so that each step compiles and tests before the next
begins. The implementer runs the verification command (§7) after each
step, not just at the end.

### Step 1 — `internal/util/loopback.go` (new file)

Create with two exported functions and their doc comments:
`SchemeOf(rawURL string) (string, error)` and
`IsLoopbackOrLinkLocal(host string) bool`, plus a tiny unexported
`HostOf(rawURL string) string` helper used by §4.9's startup warning
(strips scheme/path, leaves `host[:port]`, brackets intact for IPv6).
Implementer notes:
- `SchemeOf` calls `url.Parse(rawURL)`; on error returns `("", err)`; on
  empty `u.Scheme` returns `("", errors.New("util: URL has no scheme"))`;
  otherwise returns `strings.ToLower(u.Scheme), nil`.
- `IsLoopbackOrLinkLocal`: strip bracket pair from `host` if present
  (`[::1]` → `::1`); if `host` contains a `:` and the part after the last
  `:` parses as a port (all digits), strip it; then check `localhost` and
  `localhost.` by string equality, else `net.ParseIP(host)` and test
  membership in the four `net.IPNet` ranges from Decision C. Construct the
  four `net.IPNet` values with `net.IPv4Mask(0xff, 0, 0, 0),
  net.IPv4Mask(0xff, 0xff, 0, 0), net.CIDRMask(128, 128),
  net.CIDRMask(10, 128)` against the well-known network bases
  (`127.0.0.0`, `169.254.0.0`, `::1`, `fe80::`).
- This file must **not import** any `internal/*` package — only `net`,
  `net/url`, `errors`, `strings`. The implementer confirms with `go vet
  ./internal/util/...` and by checking the import graph stays a DAG.

### Step 2 — `internal/util/util_test.go` (extend)

Add `TestSchemeOf` and `TestIsLoopbackOrLinkLocal` table-driven tests
covering every case named in Decision J. Positive (returns true / no
error) and negative (returns false / errors) cases in the same table, with
distinct `name` fields so `-run` and `-v` output is readable. Include:
`localhost`, `localhost.`, `127.0.0.1`, `127.255.255.255`,
`[::1]`, `[::1]:8080`, `fe80::1`, `169.254.10.20`, `192.168.1.10`,
`10.0.0.1`, `example.com`, `192.168.1.10:3000`,
`http://x`, `https://x`, `gopher://x`, `//noscheme`,
`not a url at all`, `""`.
Run `go test ./internal/util/...` — must pass before Step 3.

### Step 3 — `internal/util/url.go` (rewrite `NormalizeBookOrbitURL`)

Restructure the body so parse-and-validate precedes the existing
string-suffix rules. The implementer's exact diff shape:
1. `u, err := url.Parse(strings.TrimSpace(raw))` — on error or empty input,
   return `""`.
2. If `u.User != nil` or `u.Fragment != ""`, return `""` (the userinfo and
   fragment rejection from §4.6).
3. Reassemble `u.String()` (this drops the rejected userinfo/fragment if
   the parser accepted them, but step 2 already returned in those cases;
   the reassemble is defensive), strip a trailing `/`, and apply the
   existing three suffix rules verbatim (collapse `/api/v1/koreader`,
   append `/api/v1` if absent).
4. Return the result.
Run `go test ./internal/util/...` — the existing `TestNormalizeBookOrbitURL`
cases must still pass unchanged. **Do not edit `util_test.go`'s existing
test table** — if any existing case fails, the rewrite is wrong, not the
test. Add new failing-input cases (`http://user:pass@nas/api/v1`,
`http://nas/api/v1#frag`, `not a url`, `""`) to the table as a separate
`TestNormalizeBookOrbitURLRejectsInvalid` test asserting each returns `""`.

### Step 4 — `internal/config/types.go` (add field)

Add `AllowInsecureTransport bool yaml:"allow_insecure_transport"` to
`BookOrbitConfig` after `DeviceID` (line 51). Add a doc comment (per §4.4)
naming exactly what it relaxes and what it does not.

### Step 5 — `internal/config/env.go` (add env constant + applyEnv line)

- In the `const (...)` block (lines 13–32), add
  `EnvBookOrbitAllowInsecureTransport = "BRIDGE_BOOKORBIT_ALLOW_INSECURE_TRANSPORT"`.
- In `applyEnv` (line 37), add
  `setBool(&c.BookOrbit.AllowInsecureTransport, EnvBookOrbitAllowInsecureTransport)`
  near the other `BookOrbit` env overlays (lines 51–56).

### Step 6 — `internal/config/load.go` (add yaml `applyValues` arm)

Add a new `case "bookorbit.allow_insecure_transport":` arm to the switch in
`applyValues` (line 77), modeled on the existing `"bridge.sync_status":`
arm (lines 143–148): `strconv.ParseBool(val)`, on error return
`fmt.Errorf("bookorbit.allow_insecure_transport: %w", err)`, on success
assign to `c.BookOrbit.AllowInsecureTransport`. The unknown-key rejection at
line 174 is unchanged and is the safety rail.

### Step 7 — `internal/config/validate.go` (the core check)

Add the new validation clauses to `Validate()` (line 16), appended to the
existing `problems []string`. The implementer writes them as three separate
clauses so each failure mode has its own message:
1. **BookOrbit URL shape:** compute `scheme, err := util.SchemeOf(c.BookOrbit.ServerURL)`.
   On `err != nil` or `scheme == ""`, append `"bookorbit.server_url is not a valid URL: <detail>"`.
   On `scheme != "http" && scheme != "https"`, append
   `"bookorbit.server_url must use http or https; got <scheme>"`.
2. **BookOrbit cleartext transport:** only when the shape check passed
   (i.e., `scheme` is `http` or `https`), and only when `scheme == "http"`:
   extract the host with `util.HostOf(c.BookOrbit.ServerURL)`; if
   `!util.IsLoopbackOrLinkLocal(host) && !c.BookOrbit.AllowInsecureTransport`,
   append `"bookorbit.server_url uses cleartext http to a non-loopback host (<host>); the x-auth-key header is the MD5 of your password (a password-equivalent credential). Use https://, or set bookorbit.allow_insecure_transport: true to acknowledge the cleartext risk."`.
3. **Readest URLs:** for both `c.Readest.SupabaseURL` and
   `c.Readest.SyncBaseURL`, compute `SchemeOf`; on error append
   `"<key> is not a valid URL: <detail>"`; on scheme `!= "https"` append
   `"<key> must use https (Supabase password grant / Bearer token travel to this endpoint)"`.
   No opt-out for Readest, per Decision E.
Import `github.com/Riffsmith/bookorbit-readest-sync/internal/util` is
already present (line 4) for `normalizeServerURL`'s delegation; the new
helpers are in the same package, so no new import line is needed.

### Step 8 — `internal/config/config_test.go` (extend)

Add tests:
- `TestValidateRejectsBadScheme`: set a default-config `cfg` with all
  required fields, `ServerURL = "gopher://nas/api/v1"`, assert `Validate()`
  returns a `*ValidationError` whose `Problems` contains the "must use
  http or https" substring.
- `TestValidateRejectsCleartextNonLoopback`: `ServerURL =
  "http://192.168.1.10:3000/api/v1"`, `AllowInsecureTransport = false`,
  assert the cleartext message is in `Problems`. Then set
  `AllowInsecureTransport = true` and assert `Validate()` returns nil for
  that clause.
- `TestValidateAcceptsLoopbackCleartext`: `ServerURL =
  "http://127.0.0.1:3000/api/v1"`, `AllowInsecureTransport = false`,
  assert `Validate()` does *not* mention the cleartext clause.
- `TestValidateRejectsReadestHttp`: set `SupabaseURL = "http://sb.example.co"`
  and `SyncBaseURL = "http://sync.example.com/api"` and assert both
  messages appear in `Problems`.
- `TestLoadEnvAllowInsecureTransport`: write a minimal valid yaml, set
  `t.Setenv(EnvBookOrbitAllowInsecureTransport, "yes")`, assert
  `cfg.BookOrbit.AllowInsecureTransport == true`. (Mirrors the existing
  `TestEnvOverridesFile` shape.)
- `TestLoadYamlAllowInsecureTransport`: write a yaml with
  `bookorbit.allow_insecure_transport: true`, assert the field is set.
- Update `setRequiredEnv` (line ~76) — it currently sets
  `EnvBookOrbitServerURL = "http://nas:8080"`. After Step 7, this URL is
  cleartext non-loopback and would fail `Validate`. Change it to
  `"https://nas:8080"` (a test value; no real DNS is done by `Validate`).
  Audit every other test that constructs a `Config` and supply
  `https://…` or loopback URLs so `Validate` passes. The implementer
  finds these by running `go test ./internal/config/...` and fixing each
  failure by changing the test URL, not by weakening the new check.

### Step 9 — `cmd/bridge/main.go` (startup warning)

- Add `"github.com/Riffsmith/bookorbit-readest-sync/internal/util"` to the
  import block.
- After the logger is built (line 83) and before `boClient.Auth(ctx)`
  (line 134), add:
  ```go
  if cfg.BookOrbit.AllowInsecureTransport {
      log.Warn("bridge: bookorbit.server_url uses cleartext http to a non-loopback host; x-auth-key (MD5 of password) will travel in cleartext",
          "url", cfg.BookOrbit.ServerURL,
          "host", util.HostOf(cfg.BookOrbit.ServerURL))
  }
  ```
  This bakes in the invariant that `AllowInsecureTransport == true`
  *implies* the warning fires (the only way the boolean is true is if the
  operator set it, and the only way setting it matters is if the URL is
  cleartext non-loopback, which `Validate` has already confirmed at this
  point). There is no missing-else: an `https://` deployment does not log
  anything, which is correct.

### Step 10 — `configs/bridge.example.yaml` (documentation)

Apply the two edits from §4.10. The implementer reads the file first
(`Read` tool) and uses `edit` to insert the new commented field after
`userkey` (line 48) and to append the transport note to the `server_url`
comment (lines 38–40).

### Step 11 — `docker-compose.yml` (documentation)

Apply the two edits from §4.11. The implementer reads the file first and
uses `edit` to change the `BRIDGE_BOOKORBIT_SERVER_URL` value (line 16) and
add the commented `BRIDGE_BOOKORBIT_ALLOW_INSECURE_TRANSPORT` placeholder
nearby.

### Step 12 — `README.md` and `systemd/bridge.service` (documentation)

Apply the one-sentence notes from §4.12 and §4.13. These are documentation
only; no test covers them, but the implementer re-reads each file after
editing to confirm the surrounding markdown/code-fence structure is intact.

### Step 13 — `docs/adr/` (decision record)

Add `docs/adr/security-hardening-decision-record.md` following the
existing ADR shape (see `docs/adr/phase-9-decision-record.md` for the
template). The record documents, after the fact: which decisions from §3
were implemented exactly as written, any decision that changed during
implementation (with the reason), the per-file change list (mirroring §6
but in past tense), and the verification commands run (§7) with their
results. The decision record is written *last*, after the verification
passes, so it accurately reflects what shipped.

---

## 7. Verification

The implementer runs these after every step that touches Go code (Steps
1–3, 4–9), and once more at the end after Step 13. All must pass:

```sh
gofmt -s -l .                       # no output (everything formatted)
go vet ./...                        # no diagnostics
go build ./...                      # builds clean
go test ./...                       # all unit tests pass
go test -race ./...                 # race-clean (the new helpers are pure
                                    #   and add no concurrency, but the
                                    #   project's convention is to run -race)
```

The implementer additionally runs a manual smoke check that does **not**
hit the network: from the repo root, attempt

```sh
BRIDGE_READEST_EMAIL=x BRIDGE_READEST_PASSWORD=x \
BRIDGE_BOOKORBIT_SERVER_URL='http://192.168.1.10:3000' \
BRIDGE_BOOKORBIT_USERNAME=x BRIDGE_BOOKORBIT_PASSWORD=x \
go run ./cmd/bridge --once --config /nonexistent
```

and confirms the bridge exits non-zero with the cleartext-transport
problem in the `ValidationError` output. Then with
`BRIDGE_BOOKORBIT_ALLOW_INSECURE_TRANSPORT=true` added, confirms the bridge
proceeds past `Validate` and logs the cleartext `WARN` before failing on
the (fake) credentials — proving the opt-in works end to end. This smoke
check is documented in the decision record (Step 13) so a future
maintainer can reproduce it.

The `go test ./internal/config/...` step is the primary correctness gate
for the new logic; if any pre-existing test fails because of the new
`Validate` clauses, the fix is to change the *test's URL* to a loopback or
`https` value, never to weaken the new check. This rule is what keeps the
security property real — a test that quietly disables the check would
re-introduce the bug.

---

## 8. Out-of-scope items this design deliberately does not address

These are the other review findings, listed here so the implementer does
not accidentally try to fix them as part of this work. Each belongs to a
separate future change with its own design.

- **#2** Memory-resident Readest password + core dumps. The mitigation
  (`LimitCORE=0` in the systemd unit, container seccomp) is small but
  touches operational hardening, not config validation.
- **#4** `BRIDGE_SUPABASE_ANON_KEY` base64 auto-decode heuristic. The anon
  key is public (the comment in `defaults.go:14-18` is explicit), so the
  practical impact is debugging-confusion, not security. Fixing it would
  mean choosing between "accept the decoded form only" and "accept the
  base64 form only" — neither is clearly better, and it is unrelated to
  #1+#3.
- **#5** `http.Client` redirect-following. A `CheckRedirect` that forbids
  cross-host redirects is a good defense-in-depth, but the request
  headers in question (`Authorization`, `x-auth-user`, `x-auth-key`) are
  already stripped by Go's `http.Client` on cross-host redirects (since
  Go 1.8), so the practical impact is small. Ship independently.
- **#6 / #7** Compose secrets hygiene and container runtime hardening.
  These are deployment-shape changes (env_file, Docker secrets, cap_drop,
  read_only, no-new-privileges) that touch the docker-compose.yml and the
  systemd unit but not the Go code. Worth a dedicated "deployment
  hardening" change.
- **#8** `govulncheck` in CI. Unrelated to the security properties of the
  bridge's own code; a CI-pipeline change.

---

## 9. Summary

This fix makes two compounding security problems loud at config load
instead of silent at request time:

- A BookOrbit URL that is unparseable, has the wrong scheme, or carries
  userinfo/fragment is rejected before any credential leaves the process.
- A BookOrbit URL that would broadcast a password-equivalent credential
  (the MD5 `x-auth-key`) in cleartext to a non-loopback host is rejected
  *unless* the operator has explicitly opted in, in which case a single
  startup `WARN` documents the decision.
- The Readest URLs (Supabase password grant, Bearer token) are required
  to be `https` outright, with no opt-out.

The change is additive: one new config field, two new pure helpers in
`internal/util`, three new `Validate` clauses, one rewritten normalizer
body, and four documentation touch-ups. No existing behavior of the sync
engine, the BookOrbit client, the Readest auth lifecycle, or the state
store changes. The test surface is table-driven and hermetic (no DNS, no
network). The opt-in is deliberately narrow (one boolean) and the opt-out
failure mode is deliberately binary (fatal at load, unless boolean set).
The implementer follows §6 in order, runs §7 after each Go-touching step,
and writes the ADR (§6 Step 13) last.
