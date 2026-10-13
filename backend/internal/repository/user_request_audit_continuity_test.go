package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

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

func TestAuditContinuityUsesStoredProofWithoutReadingPriorPayload(t *testing.T) {
	r := auditConversationTestRepository(t)
	ctx := context.Background()
	first := &service.UserRequestAudit{UserID: 1, APIKeyID: 2, LogicalKey: "proof-first", RequestChatML: continuityChat("user", "question"), Status: "received"}
	auditCreate(t, r, first)
	answer := continuityChat("assistant", "answer")
	if err := r.Complete(ctx, &service.UserRequestAuditCompletion{LogicalKey: first.LogicalKey, ResponseChatML: answer, Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	path := auditRequestPath(t, r, first.ID)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	second := &service.UserRequestAudit{UserID: 1, APIKeyID: 2, LogicalKey: "proof-second", RequestChatML: first.RequestChatML + answer + continuityChat("user", "next"), Status: "received"}
	auditCreate(t, auditReopen(r), second)
	rows, total, err := r.ListConversations(ctx, service.UserRequestAuditFilter{})
	if err != nil || total != 1 || len(rows) != 1 || rows[0].RequestCount != 2 {
		t.Fatalf("proof association reread missing payload: %+v %d %v", rows, total, err)
	}
	if stored := auditStoredRequestChat(t, r, second.ID); stored != answer+continuityChat("user", "next") {
		t.Fatalf("missing prior payload prevented SQL-only delta capture: %q", stored)
	}
	if _, err := r.GetByID(ctx, second.ID); err == nil {
		t.Fatal("human read concealed missing prefix evidence")
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := auditReopen(r).GetByID(ctx, second.ID)
	if err != nil || got.RequestChatML != second.RequestChatML {
		t.Fatalf("restored prefix did not reconstruct child: %+v %v", got, err)
	}
}

func auditStoredRequestChat(t *testing.T, r *FileUserRequestAuditRepository, id int64) string {
	t.Helper()
	raw, err := os.ReadFile(auditRequestPath(t, r, id))
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		ChatML string `json:"chatml"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	return payload.ChatML
}

func TestAuditIncrementalCapturePreservesEveryNewTurn(t *testing.T) {
	r := auditConversationTestRepository(t)
	ctx := context.Background()
	first := &service.UserRequestAudit{UserID: 7, APIKeyID: 8, SessionID: "incremental", LogicalKey: "first",
		RequestChatML: continuityChat("system", "instructions  with\nspacing") + continuityChat("user", "历史问题🙂"), Status: "received"}
	auditCreate(t, r, first)
	answer := continuityChat("assistant", "<think>reasoning evidence</think>\n{\"tool_calls\":[{\"id\":\"a\"},{\"id\":\"b\"}]}")
	if err := r.Complete(ctx, &service.UserRequestAuditCompletion{LogicalKey: first.LogicalKey, ResponseChatML: answer, Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	delta := continuityChat("tool", "{\"call_id\":\"a\",\"output\":\"tool-result-A\"}") +
		continuityChat("tool", "{\"call_id\":\"b\",\"output\":\"tool-result-B\"}") +
		continuityChat("user", "first new user") + continuityChat("user", "second new user")
	second := &service.UserRequestAudit{UserID: 7, APIKeyID: 8, SessionID: "incremental", LogicalKey: "second",
		RequestChatML: first.RequestChatML + answer + delta, Status: "received"}
	auditCreate(t, auditReopen(r), second)
	if stored := auditStoredRequestChat(t, r, second.ID); stored != answer+delta {
		t.Fatalf("historical request prefix was duplicated or new turns lost: stored=%q want=%q", stored, answer+delta)
	}
	// Later completion chunks cannot change the prefix captured by the child.
	if err := r.Complete(ctx, &service.UserRequestAuditCompletion{LogicalKey: first.LogicalKey, ResponseChatML: continuityChat("assistant", "late append"), Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	reopened := auditReopen(r)
	got, err := reopened.GetByID(ctx, second.ID)
	if err != nil || got.RequestChatML != second.RequestChatML {
		t.Fatalf("reconstructed evidence changed: %+v %v", got, err)
	}
	var out bytes.Buffer
	if n, err := reopened.ExportConversations(ctx, service.UserRequestAuditFilter{}, []int64{second.ID}, &out, "jsonl"); err != nil || n != 1 {
		t.Fatalf("export: %d %v", n, err)
	}
	var exported struct {
		Requests []*service.UserRequestAudit `json:"requests"`
	}
	if err := json.Unmarshal(out.Bytes(), &exported); err != nil {
		t.Fatal(err)
	}
	if len(exported.Requests) != 2 || exported.Requests[1].RequestChatML != second.RequestChatML {
		t.Fatalf("export lost raw multi-turn evidence: %+v", exported.Requests)
	}
}

func TestAuditIncrementalScopeAndSnapshotFallback(t *testing.T) {
	for _, kind := range []string{"different-user", "different-key", "modified-system", "whitespace-only", "truncated-history", "partial-message", "capture-truncated-metadata", "capture-truncated-text"} {
		t.Run(kind, func(t *testing.T) {
			r := auditConversationTestRepository(t)
			first := &service.UserRequestAudit{UserID: 7, APIKeyID: 8, SessionID: "same-session", LogicalKey: "first",
				RequestChatML:  continuityChat("system", "original") + continuityChat("user", "same prompt"),
				ResponseChatML: continuityChat("assistant", "answer"), Status: "completed"}
			auditCreate(t, r, first)
			next := &service.UserRequestAudit{UserID: 7, APIKeyID: 8, SessionID: first.SessionID, LogicalKey: "next",
				RequestChatML: first.RequestChatML + first.ResponseChatML + continuityChat("user", "next")}
			switch kind {
			case "different-user":
				next.UserID++
			case "different-key":
				next.APIKeyID++
			case "modified-system":
				next.RequestChatML = strings.Replace(next.RequestChatML, "original", "modified", 1)
			case "whitespace-only":
				next.RequestChatML = strings.Replace(next.RequestChatML, "same prompt", "same  prompt", 1)
			case "truncated-history":
				next.RequestChatML = continuityChat("user", "same prompt") + continuityChat("user", "next")
			case "partial-message":
				next.RequestChatML = strings.TrimSuffix(first.RequestChatML, "<|im_end|>\n") + " extended<|im_end|>\n"
			case "capture-truncated-metadata":
				next.Metadata = map[string]any{"request_chatml_truncated": true}
			case "capture-truncated-text":
				next.RequestChatML += "...[truncated]"
			}
			auditCreate(t, r, next)
			if stored := auditStoredRequestChat(t, r, next.ID); stored != next.RequestChatML {
				t.Fatalf("unproven/scope-crossing prefix referenced: got=%q want=%q", stored, next.RequestChatML)
			}
			got, err := auditReopen(r).GetByID(context.Background(), next.ID)
			if err != nil || got.RequestChatML != next.RequestChatML {
				t.Fatalf("snapshot altered: %+v %v", got, err)
			}
			if kind == "different-user" || kind == "different-key" {
				conversation, err := r.GetConversation(context.Background(), next.ID)
				if err != nil || conversation.RequestCount != 1 {
					t.Fatalf("same prompt merged across owner scope: %+v %v", conversation, err)
				}
			}
		})
	}
}

func TestAuditIncrementalBranchesSurviveRetentionAndClear(t *testing.T) {
	r := auditConversationTestRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()
	root := &service.UserRequestAudit{UserID: 7, APIKeyID: 8, LogicalKey: "root",
		RequestChatML: continuityChat("user", "root question"), ResponseChatML: continuityChat("assistant", "root answer"),
		Status: "completed", ExpiresAt: now.Add(-time.Hour)}
	auditCreate(t, r, root)
	var branches []*service.UserRequestAudit
	for i := range 2 {
		delta := continuityChat("user", fmt.Sprintf("branch-%d", i))
		child := &service.UserRequestAudit{UserID: 7, APIKeyID: 8, LogicalKey: fmt.Sprintf("child-%d", i),
			RequestChatML: root.RequestChatML + root.ResponseChatML + delta, ExpiresAt: now.Add(time.Hour)}
		auditCreate(t, r, child)
		branches = append(branches, child)
	}
	if _, err := r.DeleteExpired(ctx, now, 100); err != nil {
		t.Fatal(err)
	}
	reopened := auditReopen(r)
	for _, child := range branches {
		got, err := reopened.GetByID(ctx, child.ID)
		if err != nil || got.RequestChatML != child.RequestChatML {
			t.Fatalf("retention broke branch %d: %+v %v", child.ID, got, err)
		}
		var out bytes.Buffer
		if _, err := reopened.ExportConversations(ctx, service.UserRequestAuditFilter{}, []int64{child.ID}, &out, "jsonl"); err != nil {
			t.Fatalf("branch export after retention: %v", err)
		}
	}
	if _, err := reopened.DeleteExpired(ctx, now.Add(2*time.Hour), 100); err != nil {
		t.Fatal(err)
	}
	if _, n, err := auditReopen(reopened).List(ctx, service.UserRequestAuditFilter{}); err != nil || n != 0 {
		t.Fatalf("expired dependency chain remained: %d %v", n, err)
	}
	first := &service.UserRequestAudit{UserID: 7, APIKeyID: 8, SessionID: "clear", LogicalKey: "clear-first", RequestChatML: root.RequestChatML}
	auditCreate(t, reopened, first)
	next := &service.UserRequestAudit{UserID: 7, APIKeyID: 8, SessionID: "clear", LogicalKey: "clear-next", RequestChatML: first.RequestChatML + continuityChat("user", "new")}
	auditCreate(t, reopened, next)
	if n, err := reopened.Clear(ctx); err != nil || n != 2 {
		t.Fatalf("clear delta chain: %d %v", n, err)
	}
	if _, err := auditReopen(reopened).GetByID(ctx, next.ID); err == nil {
		t.Fatal("cleared delta chain resurrected")
	}
	replacement := &service.UserRequestAudit{UserID: 7, APIKeyID: 8, SessionID: "clear", LogicalKey: "replacement", RequestChatML: next.RequestChatML}
	auditCreate(t, reopened, replacement)
	if replacement.ID <= next.ID || auditStoredRequestChat(t, reopened, replacement.ID) != replacement.RequestChatML {
		t.Fatal("new capture references cleared history")
	}
}

func TestAuditIncrementalRejectsTamperedPrefix(t *testing.T) {
	r := auditConversationTestRepository(t)
	first := &service.UserRequestAudit{UserID: 1, APIKeyID: 2, SessionID: "tamper", LogicalKey: "first", RequestChatML: continuityChat("user", "original")}
	auditCreate(t, r, first)
	next := &service.UserRequestAudit{UserID: 1, APIKeyID: 2, SessionID: "tamper", LogicalKey: "next", RequestChatML: first.RequestChatML + continuityChat("user", "next")}
	auditCreate(t, r, next)
	raw, err := json.Marshal(map[string]string{"chatml": continuityChat("user", "replaced")})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(auditRequestPath(t, r, first.ID), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	reopened := auditReopen(r)
	if _, err := reopened.GetByID(context.Background(), next.ID); err == nil {
		t.Fatal("reader silently accepted changed prefix evidence")
	}
	var out bytes.Buffer
	if _, err := reopened.ExportConversations(context.Background(), service.UserRequestAuditFilter{}, []int64{next.ID}, &out, "jsonl"); err == nil {
		t.Fatal("export silently accepted changed prefix evidence")
	}
}

func TestAuditIncrementalReadsPreMigrationSnapshot(t *testing.T) {
	r := auditConversationTestRepository(t)
	row := &service.UserRequestAudit{UserID: 1, APIKeyID: 2, SessionID: "legacy", LogicalKey: "legacy", RequestChatML: continuityChat("user", "legacy evidence")}
	auditCreate(t, r, row)
	// Migration defaults leave existing full payloads without a byte proof.
	if _, err := r.db.Exec(`UPDATE user_request_audit_request_index SET request_parent_id=NULL,request_prefix_bytes=0,request_bytes=0,request_sha256='' WHERE id=$1`, row.ID); err != nil {
		t.Fatal(err)
	}
	reopened := auditReopen(r)
	got, err := reopened.GetByID(context.Background(), row.ID)
	if err != nil || got.RequestChatML != row.RequestChatML {
		t.Fatalf("pre-migration evidence unreadable: %+v %v", got, err)
	}
	next := &service.UserRequestAudit{UserID: 1, APIKeyID: 2, SessionID: "legacy", LogicalKey: "next", RequestChatML: row.RequestChatML + continuityChat("user", "next")}
	auditCreate(t, reopened, next)
	if stored := auditStoredRequestChat(t, reopened, next.ID); stored != next.RequestChatML {
		t.Fatal("legacy snapshot without byte proof used as a delta parent")
	}
}

func TestAuditIncrementalIdenticalRetryStoresHistoryOnce(t *testing.T) {
	r := auditConversationTestRepository(t)
	first := &service.UserRequestAudit{UserID: 1, APIKeyID: 2, SessionID: "retry", LogicalKey: "first",
		RequestChatML: continuityChat("system", "stable instructions") + continuityChat("user", "retry question"), Status: "failed"}
	auditCreate(t, r, first)
	second := *first
	second.ID, second.LogicalKey, second.Status = 0, "retry", "received"
	auditCreate(t, auditReopen(r), &second)
	if stored := auditStoredRequestChat(t, r, second.ID); stored != "" {
		t.Fatalf("identical retry duplicated request history: %q", stored)
	}
	got, err := auditReopen(r).GetByID(context.Background(), second.ID)
	if err != nil || got.RequestChatML != first.RequestChatML || got.Status != "received" {
		t.Fatalf("empty delta lost retry evidence: %+v %v", got, err)
	}
}

func TestAuditIncrementalUnknownToolParentPreservesExactSuffix(t *testing.T) {
	r := auditConversationTestRepository(t)
	first := &service.UserRequestAudit{UserID: 1, APIKeyID: 2, SessionID: "future-tool", LogicalKey: "first",
		RequestChatML: continuityChat("user", "run tools") +
			continuityChat("assistant", `{"tool_calls":[{"id":"a"},{"id":"b"}]}`) +
			continuityChat("tool", `{"call_id":"a","output":"first result"}`) +
			continuityChat("tool", `{"call_id":"b","output":"second result"}`)}
	auditCreate(t, r, first)
	delta := continuityChat("user", "continue") + continuityChat("tool", `{"future_extension":{"data":"new evidence"}}`)
	next := &service.UserRequestAudit{UserID: 1, APIKeyID: 2, SessionID: first.SessionID, LogicalKey: "next", RequestChatML: first.RequestChatML + delta}
	auditCreate(t, auditReopen(r), next)
	if stored := auditStoredRequestChat(t, r, next.ID); stored != delta {
		t.Fatalf("unfamiliar tool schema blocked exact-byte prefix reuse: got=%q want=%q", stored, delta)
	}
	got, err := auditReopen(r).GetByID(context.Background(), next.ID)
	if err != nil || got.RequestChatML != next.RequestChatML {
		t.Fatalf("unknown tool evidence changed: %+v %v", got, err)
	}
}
