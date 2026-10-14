package repository

import (
	"context"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/service"
)

// Paging a conversation must be a pure size optimization: reading windows of two
// and concatenating them has to yield exactly the message stream a full read
// produces, including the replay deduplication at every window boundary.
func TestAuditConversationWindowsConcatenateToFullTranscript(t *testing.T) {
	r := auditConversationTestRepository(t)
	ctx := context.Background()
	group := int64(4)
	history := ""
	want := make([]*service.UserRequestAudit, 0, 7)
	for i := range 7 {
		history += continuityChat("user", string(rune('a'+i)))
		row := &service.UserRequestAudit{
			UserID: 10, APIKeyID: 20, GroupID: &group, Protocol: "openai_responses",
			LogicalKey:    string(rune('a' + i)),
			ResponseID:    "resp",
			RequestChatML: history, Status: "completed",
		}
		auditCreate(t, r, row)
		if err := r.Complete(ctx, &service.UserRequestAuditCompletion{LogicalKey: row.LogicalKey, ResponseChatML: continuityChat("assistant", string(rune('A'+i))), Status: "completed"}); err != nil {
			t.Fatal(err)
		}
		history += continuityChat("assistant", string(rune('A'+i)))
		want = append(want, row)
	}
	full, err := r.GetConversation(ctx, want[0].ID)
	if err != nil || full.RequestCount != 7 {
		t.Fatalf("full read: %+v %v", full, err)
	}
	if full.RequestStart != 1 || full.RequestEnd != 7 {
		t.Fatalf("full read bounds: %d-%d", full.RequestStart, full.RequestEnd)
	}
	var paged []service.UserRequestAuditMessage
	for offset := 0; offset < full.RequestCount; {
		window, err := r.GetConversationWindow(ctx, want[0].ID, offset, 2)
		if err != nil {
			t.Fatalf("window at %d: %v", offset, err)
		}
		if window.RequestStart != offset+1 {
			t.Fatalf("window start at %d: %d", offset, window.RequestStart)
		}
		paged = append(paged, window.Messages...)
		offset = window.RequestEnd
	}
	if len(paged) != len(full.Messages) {
		t.Fatalf("paged message count %d != full %d", len(paged), len(full.Messages))
	}
	for i := range paged {
		if paged[i].Role != full.Messages[i].Role || paged[i].Kind != full.Messages[i].Kind || paged[i].Content != full.Messages[i].Content || paged[i].RequestID != full.Messages[i].RequestID {
			t.Fatalf("paged message %d differs: %+v vs %+v", i, paged[i], full.Messages[i])
		}
	}
}

// A single-request detail must not rebuild the rest of the conversation, so it
// reads only that request's own evidence while still reporting its position.
func TestAuditRequestDetailReadsOnlyItsOwnRequest(t *testing.T) {
	r := auditConversationTestRepository(t)
	ctx := context.Background()
	group := int64(4)
	root := &service.UserRequestAudit{UserID: 10, APIKeyID: 20, GroupID: &group, Protocol: "openai_responses", LogicalKey: "detail-root", RequestChatML: continuityChat("user", "one"), Status: "completed"}
	auditCreate(t, r, root)
	root.ResponseChatML = continuityChat("assistant", "ONE")
	if err := r.Complete(ctx, &service.UserRequestAuditCompletion{LogicalKey: root.LogicalKey, ResponseChatML: root.ResponseChatML, Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	child := &service.UserRequestAudit{UserID: 10, APIKeyID: 20, GroupID: &group, Protocol: "openai_responses", LogicalKey: "detail-child", RequestChatML: root.RequestChatML + root.ResponseChatML + continuityChat("user", "two"), Status: "completed"}
	auditCreate(t, r, child)
	child.ResponseChatML = continuityChat("assistant", "TWO")
	if err := r.Complete(ctx, &service.UserRequestAuditCompletion{LogicalKey: child.LogicalKey, ResponseChatML: child.ResponseChatML, Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	got, err := r.GetByID(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RequestChatML != child.RequestChatML || got.ResponseChatML != child.ResponseChatML {
		t.Fatalf("detail evidence: got=%q/%q want=%q/%q", got.RequestChatML, got.ResponseChatML, child.RequestChatML, child.ResponseChatML)
	}
	if got.RequestCount != 2 {
		t.Fatalf("detail conversation size: %d", got.RequestCount)
	}
	// The readable conversation moved to the paged reader, so a detail must not
	// carry a rebuilt transcript of every other request.
	if got.ChatML != "" {
		t.Fatalf("detail rebuilt the conversation transcript: %q", got.ChatML)
	}
}
