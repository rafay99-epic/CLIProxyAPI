# rafay patches on CLIProxyAPI

Base: upstream tag `v8.0.6`. Each patch applies on its own; apply 0001 before 0002 when using both.

Policy: these patches live on `rafay/main` only and are never sent upstream. Upstream
releases are reviewed by hand; security fixes are always ported, everything else is optional.

Why: Anthropic prompt caches are per account. Session affinity pins a client session to
one OAuth account, but upstream moved sessions on failures that are not the account's
fault (one network blip cost 1.27M cache-write tokens) and dropped every binding on any
routing config reload.

## 0001-same-account-retry.patch

With the session-affinity selector active, a failure that is not credential-scoped is
retried on the same auth up to 2 more times (500ms, then 1.5s; aborts on ctx done) before
the request rotates. Retryable: pre-response transport errors, 500/502/503/504/520-526/529.
Never retried: client cancellation, 401/403, credential-scoped 429 (unified 5h/7d
rejection), quota and request-scoped errors. Other selectors keep upstream behavior.

If retries run out and the request rotates, the session binding stays on the original
auth: the loop marks the auth in `opts.Metadata`, `Pick` serves a fallback without
`bind()`, and `OnResult` records nothing for that fallback. Genuinely unavailable bound
auths (credential cooldown, disabled, removed) still rebind as before.

529 no longer cools the auth/model. 5xx/529 no longer delete the session binding in
`OnResult`; 5xx keeps its existing 60s model cooldown, so a session whose account keeps
returning 5xx still moves on the next request while that cooldown lasts.

Streams retry only before the first chunk reaches the client (the existing
`executeStreamWithModelPool` contract).

Only the first credential of a request gets same-account retries (it holds the session's
prompt cache). Fallbacks rotate immediately, so a provider-wide outage costs one retry
budget per request. Test: `TestSameAuthRetryOnlyRetriesTheFirstCredential`.

Files:
- `sdk/cliproxy/auth/conductor_same_auth_retry.go` (new): classification, backoff, retry helper
- `sdk/cliproxy/auth/conductor_execution.go`: hook into execute, count, and stream loops
- `sdk/cliproxy/auth/selector.go`: Pick / pickLCP fallback without rebind; OnResult guards
- `sdk/cliproxy/auth/conductor_cooldown.go`: 529 skips credential cooldown
- `sdk/cliproxy/executor/types.go`: two metadata keys
- `sdk/cliproxy/auth/conductor_same_auth_retry_test.go` (new): tests
- `sdk/cliproxy/auth/session_affinity_metadata_test.go`: two upstream tests used 500 as a
  stand-in for a credential failure; switched to 403 to keep their intent

## 0002-keep-bindings-on-reload.patch

`Manager.SetSelector` replacing one `SessionAffinitySelector` with another now hands the
old session cache and Merkle LCP matcher to the new selector (new TTL applies on the next
refresh) instead of stopping them. Changing routing strategy or TTL keeps warm sessions.

Files:
- `sdk/cliproxy/auth/conductor_selection.go`: adopt before publishing, skip old Stop
- `sdk/cliproxy/auth/selector.go`: `adoptBindings`
- `sdk/cliproxy/auth/session_cache.go`: `setTTL`; cleanup loop reads TTL under lock
- `sdk/cliproxy/session/lcp.go`: `MerklePrefixMatcher.SetTTL`
- `sdk/cliproxy/auth/selector_reload_bindings_test.go` (new): test

Overlap: both patches touch `selector.go` in separate hunks.

## Re-apply onto a new upstream tag

```sh
git checkout <new-tag>            # or rebase your fork branch onto it
git apply --3way patches-rafay/0001-same-account-retry.patch
git apply --3way patches-rafay/0002-keep-bindings-on-reload.patch
go build ./... && go test ./sdk/cliproxy/auth/ ./sdk/cliproxy/session/ ./sdk/cliproxy/
```

`--3way` needs the upstream v8.0.6 blobs in the local object store (they are when the
fork has the tag). Resolve conflicts, rerun the tests, then regenerate the patches with
`git diff -- <files>`.
