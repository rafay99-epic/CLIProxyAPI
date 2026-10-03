package auth

import (
	"slices"
	"testing"
	"time"

	cliproxysession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
)

func TestSetSelectorKeepsSessionAffinityBindings(t *testing.T) {
	previous := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &RoundRobinSelector{}, TTL: time.Hour})
	t.Cleanup(previous.Stop)
	manager := NewManager(nil, previous, nil)

	const sessionKey = "mixed::claude:sess-reload::claude-model"
	previous.cache.Set(sessionKey, "auth-a")
	turns := []cliproxysession.CanonicalTurn{
		{Role: "user", Parts: []cliproxysession.CanonicalPart{{Kind: "text", Value: "turn 1"}}},
		{Role: "assistant", Parts: []cliproxysession.CanonicalPart{{Kind: "text", Value: "ack 1"}}},
		{Role: "user", Parts: []cliproxysession.CanonicalPart{{Kind: "text", Value: "turn 2"}}},
	}
	lcp := previous.matcher.BindWithResult("lcp:v1::claude::claude-model::caller", turns, "auth-b")
	if lcp.SessionID == "" {
		t.Fatal("LCP bind returned no session ID")
	}

	// A routing reload installs a fresh selector with a different TTL and fallback.
	next := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillFirstSelector{}, TTL: 3 * time.Hour})
	t.Cleanup(next.Stop)
	manager.SetSelector(next)

	if manager.Selector() != next {
		t.Fatal("SetSelector did not install the new selector")
	}
	if got, ok := next.cache.GetAndRefresh(sessionKey); !ok || got != "auth-a" {
		t.Fatalf("explicit binding after reload = %q, %v; want auth-a", got, ok)
	}
	if authIDs, _, ok := next.matcher.LookupSession(lcp.SessionID); !ok || !slices.Contains(authIDs, "auth-b") {
		t.Fatalf("LCP binding after reload = %v, %v; want auth-b", authIDs, ok)
	}
	next.cache.mu.RLock()
	expiresAt := next.cache.entries[sessionKey].expiresAt
	next.cache.mu.RUnlock()
	if remaining := time.Until(expiresAt); remaining < 2*time.Hour {
		t.Fatalf("refreshed binding expires in %v, want the new 3h TTL", remaining)
	}
}
