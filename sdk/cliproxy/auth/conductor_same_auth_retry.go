package auth

import (
	"context"
	"net/http"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// sameAuthRetryBackoff is the wait before each extra attempt on the same credential
// after a failure that is not the credential's fault. Its length is the number of
// extra attempts. Tests shorten it.
var sameAuthRetryBackoff = []time.Duration{500 * time.Millisecond, 1500 * time.Millisecond}

// isNonCredentialUpstreamStatus reports upstream statuses that describe provider or
// edge trouble rather than a problem with the selected credential.
func isNonCredentialUpstreamStatus(status int) bool {
	switch status {
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout,
		520, 521, 522, 523, 524, 525, 526, 529:
		return true
	default:
		return false
	}
}

// isSameAuthRetryableError reports failures worth repeating on the same credential:
// pre-response transport errors and provider-side 5xx/529. Credential-scoped and
// request-scoped failures, and client cancellation, never qualify.
func isSameAuthRetryableError(err error) bool {
	if err == nil || isCredentialScopedError(err) || isRequestScopedError(err) || isRequestInvalidError(err) {
		return false
	}
	if status := statusCodeFromError(err); status != 0 {
		return isNonCredentialUpstreamStatus(status)
	}
	return isTransientTransportError(err)
}

// shouldRetrySameAuth gates same-auth retries to session-affinity routing, where moving
// a session to another account throws away its per-account prompt cache. Other
// selectors keep rotating immediately.
func (m *Manager) shouldRetrySameAuth(ctx context.Context, err error) bool {
	if m == nil || m.HomeEnabled() || (ctx != nil && ctx.Err() != nil) {
		return false
	}
	if _, ok := m.Selector().(*SessionAffinitySelector); !ok {
		return false
	}
	return isSameAuthRetryableError(err)
}

// retrySameAuth re-runs attempt on the same credential while it keeps failing with a
// same-auth-retryable error, waiting sameAuthRetryBackoff between tries. When the retries
// run out it marks authID in opts so the session binding survives this request rotating
// away. A context error ends the waits early and is returned as the error.
//
// Only the first credential of a request is retried: it is the one holding the session's
// prompt cache. Once it has been given up on, fallbacks rotate immediately so a
// provider-wide outage costs one retry budget per request, not one per account.
func retrySameAuth[T any](ctx context.Context, m *Manager, opts *cliproxyexecutor.Options, authID string, result T, err error, attempt func() (T, error)) (T, error) {
	if exhausted, _ := opts.Metadata[cliproxyexecutor.SessionAffinityRetryExhaustedMetadataKey].(map[string]struct{}); len(exhausted) > 0 {
		return result, err
	}
	for _, wait := range sameAuthRetryBackoff {
		if !m.shouldRetrySameAuth(ctx, err) {
			return result, err
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return result, ctx.Err()
		case <-timer.C:
		}
		result, err = attempt()
	}
	if m.shouldRetrySameAuth(ctx, err) {
		excluded, _ := opts.EnsureMetadata()[cliproxyexecutor.SessionAffinityRetryExhaustedMetadataKey].(map[string]struct{})
		if excluded == nil {
			excluded = make(map[string]struct{})
			opts.Metadata[cliproxyexecutor.SessionAffinityRetryExhaustedMetadataKey] = excluded
		}
		excluded[authID] = struct{}{}
	}
	return result, err
}

// sameAuthRetryExhausted reports whether authID left this request only because
// same-auth retries ran out on failures that were not its fault.
func sameAuthRetryExhausted(metadata map[string]any, authID string) bool {
	excluded, _ := metadata[cliproxyexecutor.SessionAffinityRetryExhaustedMetadataKey].(map[string]struct{})
	_, ok := excluded[authID]
	return ok
}
