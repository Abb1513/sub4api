package service

import (
	"net/http"
	"strings"
)

func stripOpenAIBedrockCodexHeaders(account *Account, headers http.Header) {
	if account == nil || !account.IsOpenAIBedrock() || headers == nil {
		return
	}
	for key := range headers {
		name := strings.ToLower(strings.TrimSpace(key))
		if name == "originator" ||
			name == "session_id" ||
			name == "session-id" ||
			name == "conversation_id" ||
			name == "thread-id" ||
			name == "thread_id" ||
			name == "x-client-request-id" ||
			name == "chatgpt-account-id" ||
			strings.HasPrefix(name, "x-codex-") {
			delete(headers, key)
		}
	}
}
