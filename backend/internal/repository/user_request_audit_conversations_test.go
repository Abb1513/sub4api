package repository

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/service"
	_ "modernc.org/sqlite"
)

func auditConversationTestRepository(t *testing.T) *FileUserRequestAuditRepository {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)
	db, err := sql.Open("sqlite", filepath.Join(dir, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	for _, statement := range []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE users(id INTEGER PRIMARY KEY, username TEXT, email TEXT)`,
		`CREATE TABLE user_request_audit_conversations(id INTEGER PRIMARY KEY, user_id BIGINT NOT NULL, api_key_id BIGINT NOT NULL, created_at TIMESTAMP NOT NULL, updated_at TIMESTAMP NOT NULL, expires_at TIMESTAMP NOT NULL, request_count INTEGER NOT NULL, summary JSON NOT NULL)`,
		`CREATE TABLE user_request_audit_request_index(id INTEGER PRIMARY KEY AUTOINCREMENT, conversation_id BIGINT REFERENCES user_request_audit_conversations(id) ON DELETE CASCADE, logical_key TEXT NOT NULL, created_at TIMESTAMP NOT NULL, metadata JSON NOT NULL, request_path TEXT NOT NULL DEFAULT '', response_path TEXT NOT NULL DEFAULT '', payload_bytes BIGINT NOT NULL DEFAULT 0, proof JSON NOT NULL DEFAULT '{}', request_parent_id BIGINT REFERENCES user_request_audit_request_index(id), request_prefix_bytes BIGINT NOT NULL DEFAULT 0, request_bytes BIGINT NOT NULL DEFAULT 0, request_sha256 TEXT NOT NULL DEFAULT '')`,
		`CREATE UNIQUE INDEX audit_logical_key ON user_request_audit_request_index(logical_key) WHERE logical_key<>''`,
		`CREATE TABLE user_request_audit_writer_lock(id INTEGER PRIMARY KEY, version INTEGER NOT NULL)`,
		`INSERT INTO user_request_audit_writer_lock VALUES(1,0)`,
		`CREATE TABLE user_request_audit_payloads(request_id BIGINT NOT NULL REFERENCES user_request_audit_request_index(id) ON DELETE CASCADE, relative_path TEXT PRIMARY KEY, kind TEXT NOT NULL CHECK(kind IN ('request','response')), byte_size BIGINT NOT NULL, ordinal INTEGER NOT NULL, UNIQUE(request_id,ordinal))`,
		`CREATE TABLE user_request_audit_payload_gc(relative_path TEXT PRIMARY KEY)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	r := NewFileUserRequestAuditRepository(db)
	r.diskUsage = func(context.Context, string) (int64, int64, error) {
		return 16 * userRequestAuditMinFreeBytes, 8 * userRequestAuditMinFreeBytes, nil
	}
	return r
}

func auditReopen(r *FileUserRequestAuditRepository) *FileUserRequestAuditRepository {
	next := NewFileUserRequestAuditRepository(r.db)
	next.diskUsage = r.diskUsage
	return next
}

func auditCreate(t *testing.T, r *FileUserRequestAuditRepository, row *service.UserRequestAudit) {
	t.Helper()
	if err := r.Create(context.Background(), row); err != nil {
		t.Fatal(err)
	}
}

func TestAuditConversationScopeAndBranches(t *testing.T) {
	r := auditConversationTestRepository(t)
	ctx := context.Background()
	group := int64(4)
	root := &service.UserRequestAudit{UserID: 10, APIKeyID: 20, GroupID: &group, Protocol: "openai_responses", LogicalKey: "root", ResponseID: "r1", RequestChatML: "root prompt", Status: "completed"}
	auditCreate(t, r, root)
	child := *root
	child.ID, child.LogicalKey, child.PreviousResponseID, child.ResponseID, child.RequestChatML = 0, "child", "r1", "r2", "child prompt"
	auditCreate(t, r, &child)
	branch := child
	branch.ID, branch.LogicalKey, branch.ResponseID, branch.RequestChatML = 0, "branch", "r3", "branch prompt"
	auditCreate(t, r, &branch)
	for i := range 2 {
		foreign := child
		foreign.ID, foreign.LogicalKey, foreign.RequestChatML = 0, fmt.Sprintf("foreign-%d", i), "foreign evidence"
		if i == 0 {
			foreign.UserID++
		} else {
			foreign.APIKeyID++
		}
		auditCreate(t, r, &foreign)
		got, err := r.GetConversation(ctx, foreign.ID)
		if err != nil || got.RequestCount != 1 {
			t.Fatalf("scope %d: %+v %v", i, got, err)
		}
	}
	got, err := r.GetConversation(ctx, child.ID)
	if err != nil || got.ID != root.ID || got.RequestCount != 3 {
		t.Fatalf("branch: %+v %v", got, err)
	}
	for _, format := range []string{"json", "jsonl"} {
		var out bytes.Buffer
		n, err := r.ExportConversations(ctx, service.UserRequestAuditFilter{}, []int64{child.ID}, &out, format)
		if err != nil || n != 1 {
			t.Fatalf("export: %d %v", n, err)
		}
		if bytes.Contains(out.Bytes(), []byte("foreign evidence")) || !bytes.Contains(out.Bytes(), []byte("branch prompt")) {
			t.Fatalf("wrong export: %s", out.Bytes())
		}
	}
	foreignUser := int64(999)
	var out bytes.Buffer
	n, err := r.ExportConversations(ctx, service.UserRequestAuditFilter{UserID: &foreignUser}, []int64{root.ID}, &out, "jsonl")
	if err != nil || n != 0 {
		t.Fatalf("export bypassed scope: %d %v", n, err)
	}
}

func TestAuditExportSelectionIntersectsFiltersAndModelFacets(t *testing.T) {
	r := auditConversationTestRepository(t)
	ctx := context.Background()
	group := int64(4)
	base := &service.UserRequestAudit{UserID: 10, APIKeyID: 20, GroupID: &group, Protocol: "anthropic_messages", RequestedModel: "claude-opus-4-1", LogicalKey: "opus", ResponseID: "o1", RequestChatML: "opus prompt", Status: "completed"}
	auditCreate(t, r, base)
	gpt := *base
	gpt.ID, gpt.LogicalKey, gpt.ResponseID, gpt.RequestedModel, gpt.Protocol, gpt.RequestChatML = 0, "gpt", "g1", "gpt-5.1-codex", "openai_responses", "gpt prompt"
	auditCreate(t, r, &gpt)

	// Selecting both conversations while filtering by model exports only the
	// model's conversation: the selection narrows the filtered set.
	var out bytes.Buffer
	n, err := r.ExportConversations(ctx, service.UserRequestAuditFilter{RequestedModel: "claude-opus-4-1"}, []int64{base.ID, gpt.ID}, &out, "jsonl")
	if err != nil || n != 1 {
		t.Fatalf("filtered selection export: %d %v", n, err)
	}
	if !bytes.Contains(out.Bytes(), []byte("opus prompt")) || bytes.Contains(out.Bytes(), []byte("gpt prompt")) {
		t.Fatalf("filter ignored during selection export: %s", out.Bytes())
	}

	facets, err := r.ListModelFacets(ctx, service.UserRequestAuditFilter{})
	if err != nil || len(facets) != 2 {
		t.Fatalf("facets: %+v %v", facets, err)
	}
	reported := map[string]service.UserRequestAuditModelFacet{}
	for _, facet := range facets {
		reported[facet.RequestedModel] = facet
	}
	if got := reported["claude-opus-4-1"]; got.Protocol != "anthropic_messages" || got.Requests != 1 || got.Conversations != 1 {
		t.Fatalf("claude facet: %+v", got)
	}
	if got := reported["gpt-5.1-codex"]; got.Protocol != "openai_responses" || got.Requests != 1 {
		t.Fatalf("openai facet: %+v", got)
	}
	// Facets honour the protocol filter so the picker can be scoped.
	scoped, err := r.ListModelFacets(ctx, service.UserRequestAuditFilter{Protocol: "openai_responses"})
	if err != nil || len(scoped) != 1 || scoped[0].RequestedModel != "gpt-5.1-codex" {
		t.Fatalf("protocol-scoped facets: %+v %v", scoped, err)
	}
}

func TestAuditConversationReaderExportAndRestart(t *testing.T) {
	r := auditConversationTestRepository(t)
	ctx := context.Background()
	first := &service.UserRequestAudit{UserID: 7, APIKeyID: 8, SessionID: "session", LogicalKey: "first", RequestChatML: continuityChat("user", "question"), Status: "received"}
	auditCreate(t, r, first)
	if err := r.Complete(ctx, &service.UserRequestAuditCompletion{LogicalKey: "first", ResponseID: "resp-1", ResponseChatML: continuityChat("assistant", "answer"), Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	second := &service.UserRequestAudit{UserID: 7, APIKeyID: 8, SessionID: "session", LogicalKey: "second", RequestChatML: continuityChat("user", "next"), Status: "failed"}
	auditCreate(t, r, second)
	reopened := auditReopen(r)
	got, err := reopened.GetByID(ctx, first.ID)
	if err != nil || got.RequestChatML != first.RequestChatML || got.ResponseChatML != continuityChat("assistant", "answer") || got.Status != "completed" || got.ResponseID != "resp-1" {
		t.Fatalf("preservation: %+v %v", got, err)
	}
	conversation, err := reopened.GetConversation(ctx, second.ID)
	if err != nil || conversation.ID != first.ID || conversation.RequestCount != 2 {
		t.Fatalf("conversation: %+v %v", conversation, err)
	}
	var out bytes.Buffer
	if _, err := reopened.ExportConversations(ctx, service.UserRequestAuditFilter{}, []int64{second.ID}, &out, "jsonl"); err != nil {
		t.Fatal(err)
	}
	var exported struct {
		Messages []service.UserRequestAuditMessage `json:"messages"`
		Requests []*service.UserRequestAudit       `json:"requests"`
	}
	if err := json.Unmarshal(out.Bytes(), &exported); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(conversation.Messages, exported.Messages) || len(exported.Requests) != 2 || exported.Requests[0].RequestChatML != first.RequestChatML || exported.Requests[0].ResponseChatML != got.ResponseChatML {
		t.Fatalf("export altered evidence: %+v", exported)
	}
}

func TestAuditConversationAccountLabels(t *testing.T) {
	r := auditConversationTestRepository(t)
	if _, err := r.db.Exec(`INSERT INTO users VALUES(7,'','who@example.com'),(8,'alice','a@b.c'),(9,'','')`); err != nil {
		t.Fatal(err)
	}
	for i, id := range []int64{7, 8, 9, 404} {
		row := &service.UserRequestAudit{UserID: id, LogicalKey: fmt.Sprint(id)}
		auditCreate(t, r, row)
		got, err := r.GetConversation(context.Background(), row.ID)
		want := []string{"who@example.com (#7)", "alice (#8)", "#9", "#404"}[i]
		if err != nil || got.UserLabel != want {
			t.Fatalf("label: %+v want=%q err=%v", got, want, err)
		}
	}
}

func TestAuditArchiveWholeConversationRetentionAndClear(t *testing.T) {
	r := auditConversationTestRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()
	old := &service.UserRequestAudit{LogicalKey: "old", SessionID: "s", CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-time.Minute)}
	fresh := &service.UserRequestAudit{LogicalKey: "fresh", SessionID: "s", ExpiresAt: now.Add(time.Hour)}
	auditCreate(t, r, old)
	auditCreate(t, r, fresh)
	if n, err := r.DeleteExpired(ctx, now, 10); err != nil || n != 0 {
		t.Fatalf("partial expiry: %d %v", n, err)
	}
	expired := &service.UserRequestAudit{LogicalKey: "expired", ExpiresAt: now.Add(-time.Minute)}
	auditCreate(t, r, expired)
	if n, err := r.DeleteExpired(ctx, now, 10); err != nil || n != 1 {
		t.Fatalf("expiry: %d %v", n, err)
	}
	if n, err := r.Clear(ctx); err != nil || n != 2 {
		t.Fatalf("clear: %d %v", n, err)
	}
	reopened := auditReopen(r)
	if _, n, err := reopened.ListConversations(ctx, service.UserRequestAuditFilter{}); err != nil || n != 0 {
		t.Fatalf("resurrection: %d %v", n, err)
	}
	next := &service.UserRequestAudit{LogicalKey: "next"}
	auditCreate(t, reopened, next)
	if next.ID <= expired.ID {
		t.Fatal("database reused deleted ID")
	}
	if err := reopened.Complete(ctx, &service.UserRequestAuditCompletion{LogicalKey: "old", ResponseChatML: "late"}); err == nil {
		t.Fatal("late completion resurrected cleared request")
	}
}

func TestAuditExportCancellation(t *testing.T) {
	r := auditConversationTestRepository(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.ExportConversations(ctx, service.UserRequestAuditFilter{}, nil, io.Discard, "jsonl"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}
