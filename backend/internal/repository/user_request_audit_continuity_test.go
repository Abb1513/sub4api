package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/service"
)

func continuityChat(role, content string) string {
	return "<|im_start|>" + role + "\n" + content + "<|im_end|>\n"
}
func continuityRows() []*service.UserRequestAudit {
	q := continuityChat("user", "q")
	a := continuityChat("assistant", "a")
	next := continuityChat("user", "next")
	return []*service.UserRequestAudit{
		{ID: 1, UserID: 1, APIKeyID: 2, Status: "completed", RequestChatML: q, ResponseChatML: a},
		{ID: 2, UserID: 1, APIKeyID: 2, Status: "completed", RequestChatML: q + a + next, ResponseChatML: continuityChat("assistant", "b")},
		{ID: 3, UserID: 1, APIKeyID: 2, Status: "completed", RequestChatML: q + a + next + continuityChat("assistant", "b") + continuityChat("user", "third"), ResponseChatML: continuityChat("assistant", "c")},
	}
}

func TestAuditConversationVerifiedContinuityAndAmbiguousBranches(t *testing.T) {
	rows := continuityRows()
	groups := groupAuditConversations(rows)
	if len(groups) != 1 || len(groups[0].rows) != 3 {
		t.Fatalf("complete exact continuation not grouped: %#v", groups)
	}
	branch := *rows[1]
	branch.ID = 4
	branch.RequestChatML = rows[0].RequestChatML + rows[0].ResponseChatML + continuityChat("user", "other branch")
	groups = groupAuditConversations(append(rows[:2], &branch))
	if len(groups) != 3 {
		t.Fatalf("ambiguous siblings joined: %d", len(groups))
	}
	rows = continuityRows()
	rows[1].APIKeyID = 3
	if got := len(groupAuditConversations(rows[:2])); got != 2 {
		t.Fatalf("scope crossed: %d", got)
	}
	rows = continuityRows()
	rows[1].RequestChatML = continuityChat("user", "q")
	rows[0].ConversationKey = "fallback:same"
	rows[1].ConversationKey = "fallback:same"
	if got := len(groupAuditConversations(rows[:2])); got != 2 {
		t.Fatalf("first-question hash joined: %d", got)
	}
}

func TestAuditConversationSessionRetriesAndScope(t *testing.T) {
	rows := []*service.UserRequestAudit{
		{ID: 1, UserID: 1, APIKeyID: 1, SessionID: "s", Status: "failed"},
		{ID: 2, UserID: 1, APIKeyID: 1, SessionID: "s", Status: "completed"},
		{ID: 3, UserID: 2, APIKeyID: 1, SessionID: "s"},
		{ID: 4, UserID: 1, APIKeyID: 2, SessionID: "s"},
	}
	groups := groupAuditConversations(rows)
	if len(groups) != 3 {
		t.Fatalf("scope groups %d", len(groups))
	}
	found := false
	for _, g := range groups {
		if len(g.rows) == 2 {
			found = g.rows[0].ID == 1 && g.rows[1].ID == 2
		}
	}
	if !found {
		t.Fatal("explicit retry session not joined")
	}
}

func TestAuditConversationReaderAndExportShareTranscript(t *testing.T) {
	r := auditConversationTestRepository(t)
	ctx := context.Background()
	rows := continuityRows()
	for _, row := range rows {
		if err := r.Create(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	conversation, err := r.GetConversation(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if conversation.ID != 1 || conversation.RequestCount != 3 {
		t.Fatalf("conversation %+v", conversation)
	}
	var buf bytes.Buffer
	if _, err := r.ExportConversations(ctx, service.UserRequestAuditFilter{}, []int64{2}, &buf, "jsonl"); err != nil {
		t.Fatal(err)
	}
	var exported struct {
		Messages []service.UserRequestAuditMessage `json:"messages"`
		Requests []*service.UserRequestAudit       `json:"requests"`
	}
	if err := json.Unmarshal(buf.Bytes(), &exported); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(conversation.Messages, exported.Messages) {
		t.Fatal("reader/export transcript differ")
	}
	if len(exported.Requests) != 3 || exported.Requests[1].RequestChatML != rows[1].RequestChatML {
		t.Fatal("raw evidence changed")
	}
}
