package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/service"
)

func TestAuditConversationMigratesLatestSnapshotsThenArchivesOriginal(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	root := filepath.Join(os.Getenv("DATA_DIR"), "user-request-audit")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	first := &service.UserRequestAudit{ID: 1, UserID: 1, APIKeyID: 2, SessionID: "s", LogicalKey: "a", CreatedAt: now, UpdatedAt: now, Status: "received", RequestChatML: "first"}
	var raw bytes.Buffer
	encoder := json.NewEncoder(&raw)
	if err := encoder.Encode(userRequestAuditFileLine{LogicalKey: "a", UserRequestAudit: first}); err != nil {
		t.Fatal(err)
	}
	completed := *first
	completed.UpdatedAt = now.Add(time.Second)
	completed.ResponseChatML = "final evidence"
	completed.Status = "completed"
	encoder.Encode(userRequestAuditFileLine{LogicalKey: "a", UserRequestAudit: &completed})
	second := &service.UserRequestAudit{ID: 2, UserID: 1, APIKeyID: 2, SessionID: "s", LogicalKey: "b", CreatedAt: now.Add(time.Minute), UpdatedAt: now.Add(time.Minute), Status: "failed", RequestChatML: "second"}
	encoder.Encode(userRequestAuditFileLine{LogicalKey: "b", UserRequestAudit: second})
	original := filepath.Join(root, "old.ndjson")
	if err := os.WriteFile(original, raw.Bytes(), 0o640); err != nil {
		t.Fatal(err)
	}
	r := NewFileUserRequestAuditRepository(nil)
	conversation, err := r.GetConversation(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if conversation.RequestCount != 2 || conversation.Requests[0].ResponseChatML != "final evidence" {
		t.Fatalf("migration %+v", conversation)
	}
	if len(conversationFiles(t, r)) != 1 {
		t.Fatal("migration did not consolidate session")
	}
	archived, err := os.ReadFile(filepath.Join(root+"-migration-backup", "old.ndjson"))
	if err != nil || !bytes.Equal(archived, raw.Bytes()) {
		t.Fatalf("original backup changed %v", err)
	}
	if _, err = os.Stat(original); !os.IsNotExist(err) {
		t.Fatal("legacy source still active")
	}
	if !r.state.Migrated {
		t.Fatal("missing migration marker")
	}
	if _, err = r.Clear(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened := NewFileUserRequestAuditRepository(nil)
	if _, total, err := reopened.ListConversations(context.Background(), service.UserRequestAuditFilter{}); err != nil || total != 0 {
		t.Fatal("migration resurrected cleared evidence")
	}
}

func TestAuditMigrationFailureKeepsOriginalEvidence(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	root := filepath.Join(os.Getenv("DATA_DIR"), "user-request-audit")
	os.MkdirAll(root, 0o750)
	original := []byte("{\"id\":1,\"logical_key\":\"a\",\"request_chatml\":\"evidence\"}\n{\"id\":")
	path := filepath.Join(root, "broken.ndjson")
	if err := os.WriteFile(path, original, 0o640); err != nil {
		t.Fatal(err)
	}
	r := NewFileUserRequestAuditRepository(nil)
	if _, _, err := r.ListConversations(context.Background(), service.UserRequestAuditFilter{}); err == nil {
		t.Fatal("invalid legacy tail silently discarded")
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatal("failed migration changed original")
	}
}

func TestAuditConversationRecoversInterruptedPublicationAndDeletion(t *testing.T) {
	r := auditConversationTestRepository(t)
	ctx := context.Background()
	row := &service.UserRequestAudit{LogicalKey: "a", SessionID: "s", RequestChatML: "before"}
	if err := r.Create(ctx, row); err != nil {
		t.Fatal(err)
	}
	changed := *row
	changed.ResponseChatML = "after"
	changed.Status = "completed"
	doc := r.makeDocument([]*service.UserRequestAudit{&changed}, row.ID)
	if err := auditAtomicJSON(r.pendingPath(row.ID), doc); err != nil {
		t.Fatal(err)
	}
	r.state.PendingIDs = []int64{row.ID}
	if err := r.saveStateLocked(); err != nil {
		t.Fatal(err)
	}
	reopened := NewFileUserRequestAuditRepository(nil)
	got, err := reopened.GetByID(ctx, row.ID)
	if err != nil || got.ResponseChatML != "after" {
		t.Fatalf("publication recovery %+v %v", got, err)
	}
	reopened.state.DeleteIDs = []int64{row.ID}
	if err = reopened.saveStateLocked(); err != nil {
		t.Fatal(err)
	}
	deleted := NewFileUserRequestAuditRepository(nil)
	if _, total, err := deleted.ListConversations(ctx, service.UserRequestAuditFilter{}); err != nil || total != 0 {
		t.Fatalf("delete recovery %d %v", total, err)
	}
}
