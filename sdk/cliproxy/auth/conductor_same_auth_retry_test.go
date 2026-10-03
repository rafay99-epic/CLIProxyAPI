package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// sameAuthRetryHarness runs requests for one Claude Code session against two claude
// credentials behind a session-affinity selector and records which auth each upstream
// call used.
type sameAuthRetryHarness struct {
	manager  *Manager
	selector *SessionAffinitySelector
	model    string
	session  string

	mu    sync.Mutex
	calls []string
	// fail returns the error for the n-th call (1-based, per auth) since the last reset.
	fail     func(ctx context.Context, authID string, n int) error
	perAuthN map[string]int
}

func newSameAuthRetryHarness(t *testing.T) *sameAuthRetryHarness {
	t.Helper()
	withQuotaCooldownEnabled(t)
	previousBackoff := sameAuthRetryBackoff
	sameAuthRetryBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { sameAuthRetryBackoff = previousBackoff })

	h := &sameAuthRetryHarness{
		model:    "claude-same-auth-" + uuid.NewString(),
		session:  uuid.NewString(),
		perAuthN: make(map[string]int),
	}
	h.selector = NewSessionAffinitySelector(&RoundRobinSelector{})
	t.Cleanup(h.selector.Stop)
	h.manager = NewManager(nil, h.selector, nil)
	h.manager.SetRetryConfig(0, 0, 0)
	for _, id := range []string{"same-auth-a-" + uuid.NewString(), "same-auth-b-" + uuid.NewString()} {
		registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: h.model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		if _, errRegister := h.manager.Register(context.Background(), &Auth{ID: id, Provider: "claude", Status: StatusActive}); errRegister != nil {
			t.Fatalf("register %s: %v", id, errRegister)
		}
	}
	attempt := func(ctx context.Context, auth *Auth) error {
		h.mu.Lock()
		h.calls = append(h.calls, auth.ID)
		h.perAuthN[auth.ID]++
		n, fail := h.perAuthN[auth.ID], h.fail
		h.mu.Unlock()
		if fail != nil {
			return fail(ctx, auth.ID, n)
		}
		return nil
	}
	h.manager.RegisterExecutor(&customStreamMockExecutor{
		identifier: "claude",
		mockCustomErrorExecutor: mockCustomErrorExecutor{executeFn: func(ctx context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
			if errAttempt := attempt(ctx, auth); errAttempt != nil {
				return cliproxyexecutor.Response{}, errAttempt
			}
			return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
		}},
		streamFn: func(ctx context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
			if errAttempt := attempt(ctx, auth); errAttempt != nil {
				return nil, errAttempt
			}
			chunks := make(chan cliproxyexecutor.StreamChunk, 1)
			chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(auth.ID)}
			close(chunks)
			return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
		},
	})
	return h
}

// run sends one request on the harness session and returns the auth that served it.
func (h *sameAuthRetryHarness) run(ctx context.Context, stream bool) (string, error) {
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Claude-Code-Session-Id": []string{h.session}}}
	req := cliproxyexecutor.Request{Model: h.model}
	if !stream {
		resp, errExecute := h.manager.Execute(ctx, []string{"claude"}, req, opts)
		return string(resp.Payload), errExecute
	}
	opts.Stream = true
	result, errStream := h.manager.ExecuteStream(ctx, []string{"claude"}, req, opts)
	if errStream != nil {
		return "", errStream
	}
	var payload []byte
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			return "", chunk.Err
		}
		payload = append(payload, chunk.Payload...)
	}
	return string(payload), nil
}

func (h *sameAuthRetryHarness) reset(fail func(ctx context.Context, authID string, n int) error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = nil
	h.perAuthN = make(map[string]int)
	h.fail = fail
}

func (h *sameAuthRetryHarness) recordedCalls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}

// boundAuth returns the single auth the session cache points at.
func (h *sameAuthRetryHarness) boundAuth(t *testing.T) string {
	t.Helper()
	h.selector.cache.mu.RLock()
	defer h.selector.cache.mu.RUnlock()
	bound := ""
	for _, entry := range h.selector.cache.entries {
		if bound != "" && entry.authID != bound {
			t.Fatalf("session bound to several auths: %s and %s", bound, entry.authID)
		}
		bound = entry.authID
	}
	return bound
}

func (h *sameAuthRetryHarness) otherAuth(authID string) string {
	for _, auth := range h.manager.List() {
		if auth.ID != authID {
			return auth.ID
		}
	}
	return ""
}

func TestSameAuthRetryPreservesSessionBinding(t *testing.T) {
	transportErr := dialRefusedError()
	overloaded := customStatusError{code: 529, msg: `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`}
	unifiedRejection := streamQuotaError{customStatusError: customStatusError{code: http.StatusTooManyRequests, msg: "rate_limit_error"}, credentialScoped: true}

	cases := []struct {
		name string
		// boundErr is returned by the bound auth on its n-th call; nil means success.
		boundErr       func(n int) error
		wantBoundCalls int
		wantFallback   bool
		wantRebind     bool
		wantNoCooldown bool
	}{
		{
			name:           "transport error then success retries same auth",
			boundErr:       func(n int) error { return map[bool]error{true: transportErr}[n == 1] },
			wantBoundCalls: 2,
			wantNoCooldown: true,
		},
		{
			name:           "529 then success retries same auth without cooldown",
			boundErr:       func(n int) error { return map[bool]error{true: overloaded}[n == 1] },
			wantBoundCalls: 2,
			wantNoCooldown: true,
		},
		{
			name:           "persistent transport failure rotates without rebinding",
			boundErr:       func(int) error { return transportErr },
			wantBoundCalls: 1 + len(sameAuthRetryBackoff),
			wantFallback:   true,
			wantNoCooldown: true,
		},
		{
			name:           "credential-scoped 429 rotates and rebinds",
			boundErr:       func(int) error { return unifiedRejection },
			wantBoundCalls: 1,
			wantFallback:   true,
			wantRebind:     true,
		},
	}
	for _, path := range []string{"execute", "stream"} {
		for _, tc := range cases {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				h := newSameAuthRetryHarness(t)
				stream := path == "stream"
				ctx := context.Background()

				warm, errWarm := h.run(ctx, stream)
				if errWarm != nil {
					t.Fatalf("warm-up request: %v", errWarm)
				}
				if got := h.boundAuth(t); got != warm {
					t.Fatalf("warm-up bound session to %q, want %q", got, warm)
				}
				other := h.otherAuth(warm)

				h.reset(func(_ context.Context, authID string, n int) error {
					if authID != warm {
						return nil
					}
					return tc.boundErr(n)
				})
				served, errRun := h.run(ctx, stream)
				if errRun != nil {
					t.Fatalf("request error = %v, calls=%v", errRun, h.recordedCalls())
				}

				wantCalls := make([]string, 0, tc.wantBoundCalls+1)
				for range tc.wantBoundCalls {
					wantCalls = append(wantCalls, warm)
				}
				wantServed := warm
				if tc.wantFallback {
					wantCalls = append(wantCalls, other)
					wantServed = other
				}
				if calls := h.recordedCalls(); !equalSessionAliases(calls, wantCalls) {
					t.Fatalf("upstream calls = %v, want %v", calls, wantCalls)
				}
				if served != wantServed {
					t.Fatalf("served by %q, want %q", served, wantServed)
				}
				wantBound := warm
				if tc.wantRebind {
					wantBound = other
				}
				if got := h.boundAuth(t); got != wantBound {
					t.Fatalf("session bound to %q after request, want %q", got, wantBound)
				}
				if tc.wantNoCooldown {
					assertNoCooldown(t, h.manager, warm, h.model)
				}
			})
		}
	}
}

func TestSameAuthRetrySkipsClientCancellation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		h := newSameAuthRetryHarness(t)
		ctx, cancel := context.WithCancel(context.Background())
		h.reset(func(context.Context, string, int) error {
			cancel()
			return context.Canceled
		})
		if _, errRun := h.run(ctx, stream); !errors.Is(errRun, context.Canceled) {
			t.Fatalf("stream=%t: error = %v, want context.Canceled", stream, errRun)
		}
		if calls := h.recordedCalls(); len(calls) != 1 {
			t.Fatalf("stream=%t: upstream calls = %v, want exactly one", stream, calls)
		}
		cancel()
	}
}

// When every account fails for provider-side reasons, only the bound (cache-holding)
// account spends the retry budget; the fallback is tried once.
func TestSameAuthRetryOnlyRetriesTheFirstCredential(t *testing.T) {
	overloaded := customStatusError{code: 529, msg: `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`}
	for _, stream := range []bool{false, true} {
		h := newSameAuthRetryHarness(t)
		ctx := context.Background()
		warm, errWarm := h.run(ctx, stream)
		if errWarm != nil {
			t.Fatalf("stream=%t: warm-up request: %v", stream, errWarm)
		}
		other := h.otherAuth(warm)
		h.reset(func(context.Context, string, int) error { return overloaded })
		if _, errRun := h.run(ctx, stream); errRun == nil {
			t.Fatalf("stream=%t: expected the request to fail when every account is overloaded", stream)
		}
		wantCalls := make([]string, 0, len(sameAuthRetryBackoff)+2)
		for range 1 + len(sameAuthRetryBackoff) {
			wantCalls = append(wantCalls, warm)
		}
		wantCalls = append(wantCalls, other)
		if calls := h.recordedCalls(); !equalSessionAliases(calls, wantCalls) {
			t.Fatalf("stream=%t: upstream calls = %v, want %v", stream, calls, wantCalls)
		}
		if got := h.boundAuth(t); got != warm {
			t.Fatalf("stream=%t: session bound to %q after outage, want %q", stream, got, warm)
		}
	}
}
