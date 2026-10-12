package service

import (
	"net/http"
	"time"

	"github.com/google/uuid"
)

type codexReplayIdentity struct {
	installationID, sessionID, windowID, turnID string
	contextWindowID                             string
	startedAt                                   time.Time
}

func newCodexReplayIdentity(installationID, sessionID, windowID string, now time.Time) (codexReplayIdentity, error) {
	identity := codexReplayIdentity{installationID: installationID, sessionID: sessionID, windowID: windowID, startedAt: now}
	for _, value := range []*string{&identity.sessionID, &identity.windowID, &identity.turnID, &identity.contextWindowID} {
		if *value == "" {
			id, err := uuid.NewV7()
			if err != nil {
				return identity, err
			}
			*value = id.String()
		}
	}
	return identity, nil
}

// buildCodexReplayRequest is shared with the hypothesis runner. It has no
// filesystem, account, network or global client-identity side effects.
func buildCodexReplayRequest(replay *CodexHypothesisReplay, model, system string, identity codexReplayIdentity, render func(string) string) (map[string]any, http.Header) {
	// 官方 Codex CLI 每次 turn 都带 window_number 与 context_window_id
	// （codex-rs core/src/session/session.rs:736-758）。探针/假设回放作为健康检查
	// 必须从一开始就产出完整 blob，否则出站改写链补齐字段后会与初始 blob 不一致。
	// 用 marshalCodexTurnMetadata（官方字段序 + ASCII 转义）而非 json.Marshal，
	// 保证 body 内嵌 blob 与 header blob 字节一致，且与官方线序相同。
	metadata, err := marshalCodexTurnMetadata(map[string]any{
		"installation_id": identity.installationID, "session_id": identity.sessionID,
		"thread_id": identity.sessionID, "turn_id": identity.turnID, "window_id": identity.windowID,
		"window_number": 0, "context_window_id": identity.contextWindowID,
		"turn_started_at_unix_ms": identity.startedAt.UnixMilli(), "request_kind": "turn",
	})
	if err != nil {
		metadata = []byte("{}")
	}
	input := make([]any, 0, len(replay.Messages)+1)
	for _, message := range replay.Messages {
		parts := make([]any, 0, len(message.Parts))
		for _, part := range message.Parts {
			parts = append(parts, map[string]string{"type": "input_text", "text": render(part)})
		}
		input = append(input, map[string]any{"role": message.Role, "content": parts})
	}
	if system != "" {
		last := input[len(input)-1]
		input[len(input)-1] = map[string]any{"role": "developer", "content": []any{map[string]string{"type": "input_text", "text": system}}}
		input = append(input, last)
	}
	body := map[string]any{
		"model": model, "store": false, "stream": true, "instructions": render(replay.Instructions), "input": input,
		"prompt_cache_key": identity.sessionID,
		"client_metadata": map[string]string{
			"session_id": identity.sessionID, "thread_id": identity.sessionID, "turn_id": identity.turnID,
			"x-codex-installation-id": identity.installationID, "x-codex-window-id": identity.windowID,
			"x-codex-turn-metadata": string(metadata),
		},
	}
	headers := http.Header{}
	for _, key := range []string{"session_id", "session-id", "thread-id", "x-client-request-id"} {
		headers.Set(key, identity.sessionID)
	}
	headers.Set("x-codex-installation-id", identity.installationID)
	headers.Set("x-codex-window-id", identity.windowID)
	headers.Set("x-codex-turn-metadata", string(metadata))
	return body, headers
}
