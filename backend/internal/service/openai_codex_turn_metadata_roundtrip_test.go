package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCodexTurnMetadataRealisticBlobRoundTripsByteIdentically proves a real
// Codex CLI turn-metadata blob survives decode -> re-marshal byte-identically
// (official field order, escaping, number format). Any diff is a detectable
// proxy tell.
func TestCodexTurnMetadataRealisticBlobRoundTripsByteIdentically(t *testing.T) {
	// Official serde declaration order + flatten extras last, & < > unescaped,
	// non-ASCII as \uXXXX. Includes workspaces (git URL w/ &), an i64 timestamp,
	// and a bool to exercise every leaf encoder.
	official := `{"installation_id":"349d3a26-cdff-471f-a694-f899c11f124a","session_id":"c27f122b-7f1b-49f4-a218-d8770439fef4","thread_id":"c27f122b-7f1b-49f4-a218-d8770439fef4","agent_name":"codex","turn_id":"f628d5d5-7d58-43b9-b74d-5eb13cec76b4","window_id":"c27f122b-7f1b-49f4-a218-d8770439fef4:0","window_number":0,"context_window_id":"019e8f71-1dc8-7f72-9fbc-c3bc6bf549f8","request_kind":"turn","thread_source":"user","sandbox":"workspace-write","sandbox_mode":"workspace-write","auto_review_enabled":false,"node_repl_auto_review_required":false,"node_repl_disabled":false,"workspaces":{"/home/u/repo":{"associated_remote_urls":{"origin":"https://host/repo?a=1&b=2"},"latest_git_commit_hash":"abc123","has_changes":true}},"turn_started_at_unix_ms":1791837233164,"analytics_enabled":true,"custom_flatten_key":"custom_value"}`

	decoded, err := decodeCodexTurnMetadata(official)
	require.NoError(t, err)
	require.NotNil(t, decoded)

	remarshaled, err := marshalCodexTurnMetadata(decoded)
	require.NoError(t, err)

	require.Equal(t, official, string(remarshaled),
		"official blob must round-trip byte-identically; any diff is a detectable proxy tell")
}

// TestCodexTurnMetadataHistoryInitializationRoundTrips proves the post-Oct-8
// official key (codex-rs responses_metadata.rs:568, declared immediately after
// request_kind at :566) is re-emitted in the official position. Before the key
// was known to codexTurnMetadataKeyOrder, orderedCodexJSONKeys treated it as a
// flatten extra and pushed it to the tail in lexicographic order — a byte-level
// proxy tell on every rewritten request from a current Codex CLI.
func TestCodexTurnMetadataHistoryInitializationRoundTrips(t *testing.T) {
	official := `{"installation_id":"349d3a26-cdff-471f-a694-f899c11f124a","session_id":"019e8f71-1dc8-7f72-9fbc-c3bc6bf549f8","thread_id":"019e8f71-1dc8-7f72-9fbc-c3bc6bf549f8","turn_id":"f628d5d5-7d58-43b9-b74d-5eb13cec76b4","window_id":"019e8f71-1dc8-7f72-9fbc-c3bc6bf549f8:1","window_number":1,"context_window_id":"019e8f71-1dc8-7f72-9fbc-c3bc6bf549f8","request_kind":"turn","history_initialization":"cold_resume","thread_source":"user","sandbox":"workspace-write","turn_started_at_unix_ms":1791837233164}`

	decoded, err := decodeCodexTurnMetadata(official)
	require.NoError(t, err)
	remarshaled, err := marshalCodexTurnMetadata(decoded)
	require.NoError(t, err)
	require.Equal(t, official, string(remarshaled),
		"history_initialization must stay between request_kind and the flatten extras")

	// Re-marshal from a Go map (the path that actually rewrites identity
	// fields) to pin position independent of input ordering.
	out, err := marshalCodexTurnMetadata(map[string]any{
		"history_initialization": "new",
		"request_kind":           "turn",
		"context_window_id":      "019e8f71-1dc8-7f72-9fbc-c3bc6bf549f8",
		"forked_from_thread_id":  "019e8f71-1dc8-7f72-9fbc-c3bc6bf549f7",
		"turn_id":                "f628d5d5-7d58-43b9-b74d-5eb13cec76b4",
	})
	require.NoError(t, err)
	require.Equal(t, `{"turn_id":"f628d5d5-7d58-43b9-b74d-5eb13cec76b4","context_window_id":"019e8f71-1dc8-7f72-9fbc-c3bc6bf549f8","request_kind":"turn","history_initialization":"new","forked_from_thread_id":"019e8f71-1dc8-7f72-9fbc-c3bc6bf549f7"}`, string(out))
}
