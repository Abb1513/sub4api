package repository

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/service"
)

func TestAuditOrphanFilesNeverBecomeDatabaseRecords(t *testing.T) {
	r := auditConversationTestRepository(t)
	for _, relative := range []string{"conversations/999.json", "payloads/orphan.json", "broken.ndjson", "state.json"} {
		path := filepath.Join(r.root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{malformed"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A directory where an orphan payload might be expected is unreadable as JSON
	// on all platforms, including Windows where chmod(000) is not reliable.
	if err := os.MkdirAll(filepath.Join(r.root, "payloads", "unreadable.json"), 0o750); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	reopened := auditReopen(r)
	if _, n, err := reopened.List(ctx, service.UserRequestAuditFilter{}); err != nil || n != 0 {
		t.Fatalf("bootstrap scanned orphan: %d %v", n, err)
	}
	if _, n, err := reopened.ListConversations(ctx, service.UserRequestAuditFilter{}); err != nil || n != 0 {
		t.Fatalf("conversation bootstrap scanned orphan: %d %v", n, err)
	}
	row := &service.UserRequestAudit{LogicalKey: "live", RequestedModel: "indexed", RequestChatML: "human evidence"}
	auditCreate(t, reopened, row)
	// Even a referenced malformed payload must not affect metadata operations.
	path := auditRequestPath(t, reopened, row.ID)
	if err := os.WriteFile(path, []byte("{malformed"), 0o600); err != nil {
		t.Fatal(err)
	}
	again := auditReopen(reopened)
	if rows, n, err := again.List(ctx, service.UserRequestAuditFilter{Q: "indexed"}); err != nil || n != 1 || len(rows) != 1 {
		t.Fatalf("list read payload: %d %v", n, err)
	}
	if _, n, err := again.ListConversations(ctx, service.UserRequestAuditFilter{}); err != nil || n != 1 {
		t.Fatalf("conversation list read payload: %d %v", n, err)
	}
	status := again.StorageStatus()
	if status.FileCount != 1 || status.TotalBytes <= 0 {
		t.Fatalf("SQL storage stats: %+v", status)
	}
	svc := service.NewUserRequestAuditService(again, nil)
	stats, err := svc.GroupStats(ctx, service.UserRequestAuditFilter{})
	if err != nil || len(stats) != 1 {
		t.Fatalf("group stats read payload: %+v %v", stats, err)
	}
	other := &service.UserRequestAudit{LogicalKey: "unrelated", RequestChatML: "unrelated evidence"}
	auditCreate(t, again, other)
	if got, err := again.GetByID(ctx, other.ID); err != nil || got.RequestChatML != "unrelated evidence" {
		t.Fatalf("unrelated read: %+v %v", got, err)
	}
	if _, err := again.GetByID(ctx, row.ID); err == nil {
		t.Fatal("human read concealed corrupt payload")
	}
	var out bytes.Buffer
	if _, err := again.ExportConversations(ctx, service.UserRequestAuditFilter{}, []int64{row.ID}, &out, "jsonl"); err == nil {
		t.Fatal("export concealed corrupt payload")
	}
	if n, err := again.Clear(ctx); err != nil || n != 2 {
		t.Fatalf("clear read payload: %d %v", n, err)
	}
	if _, n, err := auditReopen(again).List(ctx, service.UserRequestAuditFilter{}); err != nil || n != 0 {
		t.Fatalf("orphan resurrection: %d %v", n, err)
	}
}

func TestAuditRepositoryWithoutDatabaseFailsClosed(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	r := NewFileUserRequestAuditRepository(nil)
	if _, _, err := r.List(context.Background(), service.UserRequestAuditFilter{}); err == nil {
		t.Fatal("nil DB silently used file authority")
	}
	if err := r.Create(context.Background(), &service.UserRequestAudit{LogicalKey: "no-db"}); err == nil {
		t.Fatal("nil DB capture succeeded")
	}
}
