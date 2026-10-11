package handler

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
)

func TestAuditResponseFieldsExtractsResponseIdentityFromSSE(t *testing.T) {
	raw := []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_123\",\"model\":\"gpt-5\"}}\n")
	id, model := auditResponseFields(raw)
	if id != "resp_123" || model != "gpt-5" {
		t.Fatalf("response identity = (%q, %q)", id, model)
	}
	if !strings.Contains(auditResponseChatML(raw), "hello") {
		t.Fatal("audit transcript omitted real response content")
	}
}

func TestAuditStreamIncompleteDistinguishesTerminalAndTruncatedSSE(t *testing.T) {
	if !auditStreamIncomplete([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n")) {
		t.Fatal("truncated stream was not marked incomplete")
	}
	if auditStreamIncomplete([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\ndata: {\"type\":\"response.completed\"}\n")) {
		t.Fatal("terminal stream was marked incomplete")
	}
	if auditStreamIncomplete([]byte("data: [DONE]\n")) {
		t.Fatal("DONE stream was marked incomplete")
	}
}

func TestAuditSessionIDPrecedenceAndNoPromptCacheKey(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	c.Request.Header.Set("session-id", "header-session")
	c.Request.Header.Set("X-Session-Id", "x-session")
	body := []byte(`{"prompt_cache_key":"must-not-be-session","session_id":"body-session"}`)
	if got := auditSessionID(c, body); got != "header-session" {
		t.Fatalf("session precedence = %q", got)
	}

	c.Request.Header.Del("session-id")
	c.Request.Header.Del("X-Session-Id")
	body = []byte(`{"metadata":{"user_id":"{\"device_id\":\"d\",\"session_id\":\"metadata-session\"}"},"session_id":"body-session","prompt_cache_key":"cache"}`)
	if got := auditSessionID(c, body); got != "metadata-session" {
		t.Fatalf("metadata session = %q", got)
	}

	body = []byte(`{"prompt_cache_key":"cache-only"}`)
	if got := auditSessionID(c, body); got != "" {
		t.Fatalf("cache key became session: %q", got)
	}
}

func TestAuditSSETerminalScannerHandlesSplitAndIgnoresTextMarker(t *testing.T) {
	terminal, tail := auditStreamChunkTerminal(nil, []byte("data: {\"type\":\"response.com"))
	if terminal || len(tail) == 0 {
		t.Fatal("partial terminal event was accepted")
	}
	terminal, tail = auditStreamChunkTerminal(tail, []byte("pleted\"}\n"))
	if !terminal || len(tail) != 0 {
		t.Fatalf("split terminal = %v tail=%q", terminal, tail)
	}
	terminal, _ = auditStreamChunkTerminal(nil, []byte("data: {\"delta\":\"[DONE]\"}\n"))
	if terminal {
		t.Fatal("text marker was treated as protocol terminal")
	}
}

func TestAuditChatMLRetainsBeyondFormer32KiBBound(t *testing.T) {
	content := strings.Repeat("x", 64*1024) + "TAIL_SENTINEL"
	body := []byte(`{"messages":[{"role":"user","content":"` + content + `"}]}`)
	projected := service.ProjectUserRequestChatML(body)
	if !strings.Contains(projected, "TAIL_SENTINEL") {
		t.Fatal("projected transcript lost content beyond former 32KiB bound")
	}
}

func TestAuditSSETerminalScannerPreservesLargeFramePrefix(t *testing.T) {
	first := []byte(`data: {"type":"response.completed","response":{"text":"` + strings.Repeat("x", 10000))
	terminal, tail := auditStreamChunkTerminal(nil, first)
	if terminal || len(tail) > 8192 {
		t.Fatal("incomplete frame accepted or prefix grew without bound")
	}
	terminal, tail = auditStreamChunkTerminal(tail, []byte(strings.Repeat("x", 10000)+"\"}}\n"))
	if !terminal || len(tail) != 0 {
		t.Fatal("large split terminal frame lost its event type")
	}
	if !auditStreamIncomplete([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"[DONE] message_stop\"}\n")) {
		t.Fatal("user-visible text masqueraded as terminal protocol evidence")
	}
	terminal, _ = auditStreamChunkTerminal(nil, []byte("data: {\"type\":\"message_stop\"}\n"))
	if !terminal {
		t.Fatal("Anthropic terminal event was lost")
	}
}
