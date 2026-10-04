# CLIProxyAPI (rafay fork)

A personal fork of [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI), the proxy core
that ships inside [CPA Desk](https://github.com/rafay99-epic/EasyCLIProxyAPI).

It is upstream v8.0.6 plus two patches that keep a client session on its account when a failure
is not the account's fault. Anthropic prompt caches are per account, so every move rewrites the
session's cache. Upstream moved sessions on any network blip (one cost 1.27M cache-write tokens)
and dropped every session binding on a routing config reload.

## Branches

| Branch | Contents |
| --- | --- |
| `rafay/main` | Upstream `v8.0.6` + the fork patches. This is the code CPA Desk builds. |
| `base/v8.0.6` | Release branch. Fork PRs from `rafay/main` merge here. |
| `main` | Upstream `main` plus this README. No fork code. |

CPA Desk pins the exact core commit and version in its `core.ref` (`8.0.6-rafay.1`).

## What changed

Both changes apply only when the session-affinity selector is active. Other routing selectors
behave exactly like upstream.

| Situation | Upstream | This fork |
| --- | --- | --- |
| Network error or 5xx before the response starts | Session moves to another account | Retried on the same account up to 2 more times (500 ms, then 1.5 s) |
| 529 overloaded | Account goes into cooldown, session moves | No cooldown, retried on the same account |
| Retries run out | Session rebinds to the fallback account | Request is served by a fallback, binding stays on the original account |
| 401, 403, account-wide 429, quota errors | Session moves | Session moves (unchanged) |
| Routing strategy or TTL changes (config reload) | Every session binding dropped | Bindings and the prefix matcher carry over, new TTL applies |

Details:

- Only the first account of a request gets same-account retries, because it holds the session's
  cache. Fallbacks rotate immediately, so a provider-wide outage costs one retry budget per
  request.
- Streams retry only before the first chunk reaches the client.
- 5xx keeps upstream's 60 second model cooldown, so an account that keeps returning 5xx still
  loses the session on the next request.
- Client cancellation is never retried.

The patches are kept as files in `patches-rafay/` on `rafay/main`, with the file list, tests and
steps for re-applying them onto a new upstream tag:
[patches-rafay/README.md](https://github.com/rafay99-epic/CLIProxyAPI/blob/rafay/main/patches-rafay/README.md).

## UI changes

None in the core. The management UI for this fork is CPA Desk, which documents its own changes
in its [README](https://github.com/rafay99-epic/EasyCLIProxyAPI#readme).

## Security

Security follows upstream. This fork has no separate security policy.

- Upstream releases after v8.0.6 are reviewed by hand. Security fixes are always ported onto
  `rafay/main`; everything else is optional.
- Report vulnerabilities in upstream code to
  [router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI). Issues are turned
  off on this fork.
- The patches change retry and session routing only. Login flows, request translation and the
  management API are untouched.

## Build and test

```sh
git checkout rafay/main
go build ./cmd/server
go test ./sdk/cliproxy/auth/ ./sdk/cliproxy/session/ ./sdk/cliproxy/
```

## Upstream docs

Providers, configuration, Docker, the management API and everything else work as upstream
documents them in the [CLIProxyAPI README](https://github.com/router-for-me/CLIProxyAPI#readme).
[中文](README_CN.md) and [日本語](README_JA.md) translations describe upstream.

## License

MIT, see [LICENSE](LICENSE). Original work by Luis Pater.
