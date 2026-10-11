package service

import (
	"reflect"
	"strings"
	"testing"
)

func transcriptChat(role, content string) string {
	return "<|im_start|>" + role + "\n" + content + "<|im_end|>\n"
}
func transcriptTexts(messages []UserRequestAuditMessage) []string {
	var out []string
	for _, m := range messages {
		if m.Kind == "text" {
			out = append(out, m.Role+":"+m.Content)
		}
	}
	return out
}

func TestAuditTranscriptHistoryAndRepeatedTurns(t *testing.T) {
	first := transcriptChat("user", "repeat")
	answer := transcriptChat("assistant", "answer")
	rows := []*UserRequestAudit{
		{ID: 1, Status: "completed", RequestChatML: first, ResponseChatML: answer},
		{ID: 2, Status: "completed", RequestChatML: first + answer + first, ResponseChatML: transcriptChat("assistant", "second")},
	}
	want := []string{"user:repeat", "assistant:answer", "user:repeat", "assistant:second"}
	if got := transcriptTexts(BuildUserRequestAuditTranscript(rows)); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
	rows[1].RequestChatML = first
	if got := transcriptTexts(BuildUserRequestAuditTranscript(rows)); !reflect.DeepEqual(got, want) {
		t.Fatalf("new identical single turn lost: %#v", got)
	}
}

func TestAuditTranscriptFailedRetryKeepsFailure(t *testing.T) {
	rows := []*UserRequestAudit{{ID: 1, Status: "failed", RequestChatML: transcriptChat("user", "q"), LastError: "disconnect"}, {ID: 2, Status: "completed", RequestChatML: transcriptChat("user", "q"), ResponseChatML: transcriptChat("assistant", "a")}}
	got := BuildUserRequestAuditTranscript(rows)
	if texts := transcriptTexts(got); !reflect.DeepEqual(texts, []string{"user:q", "assistant:a"}) {
		t.Fatalf("retry texts %#v", texts)
	}
	found := false
	for _, m := range got {
		found = found || m.Kind == "error" && strings.Contains(m.Content, "disconnect")
	}
	if !found {
		t.Fatal("failed attempt omitted")
	}
}

func TestAuditTranscriptResponsesSplitToolsAndFinal(t *testing.T) {
	raw := `data: {"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"actual thought"}

data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"c1","name":"lookup","arguments":""}}

data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"q\":"}

data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"\"x\"}"}

data: {"type":"response.completed","response":{"output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"actual thought"}]},{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{\"q\":\"x\"}"}]}}

`
	got := auditDecodeWire(raw, 7)
	if len(got) != 2 || got[0].Kind != "think" || got[0].Reasoning != "actual thought" || got[1].Kind != "tool_call" || got[1].Content != `{"q":"x"}` || got[1].CallID != "c1" {
		t.Fatalf("blocks %#v", got)
	}
	withoutFinal := raw[:strings.Index(raw, "data: {\"type\":\"response.completed\"")]
	got = auditDecodeWire(withoutFinal, 7)
	if got[1].Content != `{"q":"x"}` || !got[1].Incomplete {
		t.Fatalf("split tools %#v", got)
	}
}

func TestAuditTranscriptProviderThinkingAndTools(t *testing.T) {
	cases := []struct {
		name, raw     string
		kind, content string
	}{
		{"chat", `{"choices":[{"message":{"role":"assistant","reasoning_content":"plan","content":"done"}}]}`, "think", "plan"},
		{"anthropic", `{"role":"assistant","content":[{"type":"thinking","thinking":"consider"},{"type":"tool_use","id":"t1","name":"run","input":{"x":1}}]}`, "think", "consider"},
		{"tool result", `{"type":"function_call_output","call_id":"c1","output":"ok"}`, "tool_result", "ok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := auditDecodeWire(tc.raw, 1)
			if got[0].Kind != tc.kind || got[0].Content != tc.content {
				t.Fatalf("%#v", got)
			}
		})
	}
	got := auditDecodeChatML(transcriptChat("assistant", "<think>real</think>answer"), "assistant", 1)
	if len(got) != 2 || got[0].Kind != "think" || got[1].Content != "answer" {
		t.Fatalf("think boundaries %#v", got)
	}
}

func TestAuditTranscriptNoInventedThinkingOrTruncatedProof(t *testing.T) {
	row := &UserRequestAudit{ID: 1, Status: "failed", RequestChatML: transcriptChat("reasoning", `{"effort":"high"}`) + "<|im_start|>system\nlarge...[truncated]", ResponseChatML: "<|assistant|>\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"}
	messages := BuildUserRequestAuditTranscript([]*UserRequestAudit{row})
	boundary := false
	for _, m := range messages {
		if m.Kind == "think" {
			t.Fatal("invented THINK")
		}
		boundary = boundary || m.Incomplete
	}
	if !boundary {
		t.Fatal("missing incomplete boundary")
	}
	proof := UserRequestAuditProof(row)
	if proof.Terminal != "" || len(proof.Prefixes) != 0 {
		t.Fatal("truncated evidence became continuity proof")
	}
	got := auditDecodeWire(`{"output":[{"type":"reasoning","encrypted_content":"secret"}]}`, 1)
	if len(got) != 1 || got[0].Kind != "metadata" {
		t.Fatalf("encrypted reasoning %#v", got)
	}
}

func TestAuditTranscriptNativeEnvelopeIsMetadata(t *testing.T) {
	row := &UserRequestAudit{ID: 1, Status: "completed", Protocol: ContentModerationProtocolTypeSafeSystemOne, RequestChatML: `{"state":{"content":"not a chat"}}`, ResponseChatML: `{"answers":[1]}`}
	got := BuildUserRequestAuditTranscript([]*UserRequestAudit{row})
	if len(got) != 2 || got[0].Kind != "metadata" || got[0].Content != row.RequestChatML || got[1].Content != row.ResponseChatML {
		t.Fatalf("native envelope %#v", got)
	}
}

func TestAuditTranscriptReplayOmitsPrivateThinkWithoutRepeatingTools(t *testing.T) {
	question := transcriptChat("user", "lookup")
	call := `{"type":"function_call","name":"search","call_id":"c1","arguments":"{}"}`
	rows := []*UserRequestAudit{
		{ID: 1, SessionID: "s", Status: "completed", RequestChatML: question, ResponseChatML: transcriptChat("assistant", "<think>first thought</think>") + transcriptChat("assistant", call)},
		{ID: 2, SessionID: "s", Status: "completed", RequestChatML: question + transcriptChat("assistant", call) + transcriptChat("tool", `{"type":"function_call_output","call_id":"c1","output":"found"}`), ResponseChatML: transcriptChat("assistant", "<think>second thought</think>answer")},
	}
	got := BuildUserRequestAuditTranscript(rows)
	var kinds, contents []string
	for _, m := range got {
		kinds = append(kinds, m.Kind)
		contents = append(contents, m.Content)
	}
	if !reflect.DeepEqual(kinds, []string{"text", "think", "tool_call", "tool_result", "think", "text"}) || !reflect.DeepEqual(contents, []string{"lookup", "first thought", "{}", "found", "second thought", "answer"}) {
		t.Fatalf("kinds=%#v contents=%#v", kinds, contents)
	}
}

func TestAuditTranscriptAnthropicSplitThinkingAndToolJSON(t *testing.T) {
	raw := `data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"observed"}}

data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"t1","name":"run","input":{}}}

data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"x\":"}}

data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"1}"}}

data: {"type":"message_stop"}

`
	got := auditDecodeWire(raw, 1)
	if len(got) != 2 || got[0].Kind != "think" || got[0].Content != "observed" || got[1].Kind != "tool_call" || got[1].Content != `{"x":1}` || got[1].CallID != "t1" || got[1].Incomplete {
		t.Fatalf("blocks %#v", got)
	}
}
