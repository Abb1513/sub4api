package repository

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/service"
)

func TestAuditConcurrentDatabaseIDs(t *testing.T) {
	r := auditConversationTestRepository(t)
	var wg sync.WaitGroup
	for i := range 24 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := r.Create(context.Background(), &service.UserRequestAudit{LogicalKey: fmt.Sprint(i), RequestedModel: "search-model", RequestChatML: "private-payload"}); err != nil {
				t.Errorf("create: %v", err)
			}
		}(i)
	}
	wg.Wait()
	rows, n, err := r.List(context.Background(), service.UserRequestAuditFilter{Q: "search-model", Page: 1, PageSize: 100})
	if err != nil || n != 24 || len(rows) != 24 {
		t.Fatalf("list: %d %d %v", len(rows), n, err)
	}
	seen := map[int64]bool{}
	for _, row := range rows {
		if row.ID <= 0 || seen[row.ID] {
			t.Fatalf("invalid ID %d", row.ID)
		}
		seen[row.ID] = true
		if row.RequestChatML != "" || row.ResponseChatML != "" || row.ChatML != "" {
			t.Fatal("list loaded cold text")
		}
	}
	if _, n, err := r.List(context.Background(), service.UserRequestAuditFilter{Q: "private-payload"}); err != nil || n != 0 {
		t.Fatalf("payload search: %d %v", n, err)
	}
}

func auditRequestPath(t *testing.T, r *FileUserRequestAuditRepository, id int64) string {
	t.Helper()
	var relative string
	if err := r.db.QueryRow(`SELECT request_path FROM user_request_audit_request_index WHERE id=$1`, id).Scan(&relative); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(r.root, filepath.FromSlash(relative))
}

func TestAuditDiskGuardProtectsRequestAndCompletion(t *testing.T) {
	r := auditConversationTestRepository(t)
	ctx := context.Background()
	healthy := r.diskUsage
	r.diskUsage = func(context.Context, string) (int64, int64, error) { return 16 * userRequestAuditMinFreeBytes, 0, nil }
	if err := r.Create(ctx, &service.UserRequestAudit{LogicalKey: "blocked"}); err == nil {
		t.Fatal("low space capture succeeded")
	}
	if _, n, err := r.List(ctx, service.UserRequestAuditFilter{}); err != nil || n != 0 {
		t.Fatalf("blocked row persisted: %d %v", n, err)
	}
	r.diskUsage = healthy
	row := &service.UserRequestAudit{LogicalKey: "resumed", RequestChatML: "request evidence"}
	auditCreate(t, r, row)
	r.diskUsage = func(context.Context, string) (int64, int64, error) { return 0, 0, fmt.Errorf("probe failure") }
	if !r.StorageStatus().RecordingPaused {
		t.Fatal("failed disk probe not paused")
	}
	if err := r.Complete(ctx, &service.UserRequestAuditCompletion{LogicalKey: "resumed", ResponseChatML: "blocked response"}); err == nil {
		t.Fatal("completion bypassed guard")
	}
	r.diskUsage = healthy
	got, err := r.GetByID(ctx, row.ID)
	if err != nil || got.RequestChatML != "request evidence" || got.ResponseChatML != "" {
		t.Fatalf("paused capture changed evidence: %+v %v", got, err)
	}
}

func TestAuditConfigPersistsAcrossRestart(t *testing.T) {
	r := auditConversationTestRepository(t)
	cfg := service.UserRequestAuditConfig{RetentionDays: 3, CleanupIntervalHours: 4}
	if err := r.SetAuditConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if got := auditReopen(r).GetAuditConfig(); !reflect.DeepEqual(got, cfg) {
		t.Fatalf("config: %+v", got)
	}
}

func TestAuditCompletionDoesNotReadMissingRequestPayload(t *testing.T) {
	r := auditConversationTestRepository(t)
	row := &service.UserRequestAudit{LogicalKey: "missing", RequestChatML: continuityChat("user", "question")}
	auditCreate(t, r, row)
	path := auditRequestPath(t, r, row.ID)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	for i, text := range []string{"first", "second"} {
		if err := r.Complete(context.Background(), &service.UserRequestAuditCompletion{LogicalKey: "missing", ResponseChatML: continuityChat("assistant", text), Status: "completed"}); err != nil {
			t.Fatalf("completion %d read old payload: %v", i, err)
		}
	}
	if _, err := r.GetByID(context.Background(), row.ID); err == nil {
		t.Fatal("human read concealed missing evidence")
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := auditReopen(r).GetByID(context.Background(), row.ID)
	if err != nil || got.RequestChatML != row.RequestChatML || got.ResponseChatML != continuityChat("assistant", "first")+continuityChat("assistant", "second") {
		t.Fatalf("completion chunks: %+v %v", got, err)
	}
}

func TestAuditExpiredPayloadDeletionNeedsOnlySQL(t *testing.T) {
	r := auditConversationTestRepository(t)
	row := &service.UserRequestAudit{LogicalKey: "expired", RequestChatML: "evidence", ExpiresAt: time.Now().Add(-time.Hour)}
	auditCreate(t, r, row)
	if err := os.WriteFile(auditRequestPath(t, r, row.ID), []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := r.DeleteExpired(context.Background(), time.Now(), 10); err != nil || n != 1 {
		t.Fatalf("delete read cold payload: %d %v", n, err)
	}
	if _, n, err := auditReopen(r).List(context.Background(), service.UserRequestAuditFilter{}); err != nil || n != 0 {
		t.Fatalf("expired resurrection: %d %v", n, err)
	}
}

func TestAuditSQLContainsMetadataNotPayload(t *testing.T) {
	r := auditConversationTestRepository(t)
	row := &service.UserRequestAudit{ID: 999999, LogicalKey: "database-id", RequestChatML: continuityChat("user", "PRIVATE_REQUEST_SENTINEL")}
	auditCreate(t, r, row)
	if row.ID == 999999 || row.ID <= 0 {
		t.Fatalf("caller controlled ID %d", row.ID)
	}
	if err := r.Complete(context.Background(), &service.UserRequestAuditCompletion{LogicalKey: row.LogicalKey, ResponseChatML: continuityChat("assistant", "PRIVATE_RESPONSE_SENTINEL"), Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	var metadata, proof, summary string
	if err := r.db.QueryRow(`SELECT metadata,proof FROM user_request_audit_request_index WHERE id=$1`, row.ID).Scan(&metadata, &proof); err != nil {
		t.Fatal(err)
	}
	if err := r.db.QueryRow(`SELECT summary FROM user_request_audit_conversations WHERE id=$1`, row.ID).Scan(&summary); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{metadata, proof, summary} {
		if strings.Contains(value, "PRIVATE_REQUEST_SENTINEL") || strings.Contains(value, "PRIVATE_RESPONSE_SENTINEL") {
			t.Fatal("SQL retained cold payload text")
		}
	}
}
