package repository

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/service"
	_ "modernc.org/sqlite"
)

func auditConversationTestRepository(t *testing.T) *FileUserRequestAuditRepository {
	t.Helper()
	t.Setenv("DATA_DIR", t.TempDir())
	r := NewFileUserRequestAuditRepository(nil)
	r.diskUsage = func(context.Context, string) (int64, int64, error) {
		return 16 * userRequestAuditMinFreeBytes, 8 * userRequestAuditMinFreeBytes, nil
	}
	return r
}
func conversationFiles(t *testing.T, r *FileUserRequestAuditRepository) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(r.root, "conversations", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestAuditConversationSessionUsesOneDurableJSON(t *testing.T) {
	r := auditConversationTestRepository(t)
	ctx := context.Background()
	first := &service.UserRequestAudit{UserID: 7, APIKeyID: 8, SessionID: "session", LogicalKey: "first", RequestChatML: continuityChat("user", "question"), Status: "received"}
	if err := r.Create(ctx, first); err != nil {
		t.Fatal(err)
	}
	files := conversationFiles(t, r)
	if len(files) != 1 {
		t.Fatalf("files %v", files)
	}
	path := files[0]
	if err := r.Complete(ctx, &service.UserRequestAuditCompletion{LogicalKey: "first", ResponseChatML: continuityChat("assistant", "answer"), Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	second := &service.UserRequestAudit{UserID: 7, APIKeyID: 8, SessionID: "session", LogicalKey: "second", RequestChatML: continuityChat("user", "next"), Status: "failed"}
	if err := r.Create(ctx, second); err != nil {
		t.Fatal(err)
	}
	if got := conversationFiles(t, r); !reflect.DeepEqual(got, []string{path}) {
		t.Fatalf("session changed files %v", got)
	}
	doc, err := readAuditConversation(path)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Version != 1 || doc.RequestCount != 2 || len(doc.Requests) != 2 || doc.Requests[0].ResponseChatML != continuityChat("assistant", "answer") || doc.LogicalKeys[fmt.Sprint(second.ID)] != "second" {
		t.Fatalf("document %+v", doc)
	}
	reopened := NewFileUserRequestAuditRepository(nil)
	got, err := reopened.GetConversation(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != first.ID || got.RequestCount != 2 || !reflect.DeepEqual(got.Messages, doc.Messages) {
		t.Fatalf("reopened %+v", got)
	}
	if stale, _ := filepath.Glob(filepath.Join(r.root, "*.ndjson")); len(stale) != 0 {
		t.Fatal("legacy writer still active")
	}
}

func TestAuditConversationBranchExportPreservesWholeScopedHistory(t *testing.T) {
	r := auditConversationTestRepository(t)
	ctx := context.Background()
	start := time.Now().Add(-time.Hour)
	rows := []*service.UserRequestAudit{
		{ID: 1, UserID: 10, APIKeyID: 20, LogicalKey: "root", ResponseID: "r1", RequestChatML: "root prompt", Status: "completed"},
		{ID: 2, UserID: 10, APIKeyID: 20, LogicalKey: "child", PreviousResponseID: "r1", ResponseID: "r2", RequestChatML: "selected needle", Status: "failed"},
		{ID: 3, UserID: 10, APIKeyID: 20, LogicalKey: "branch", PreviousResponseID: "r1", ResponseID: "r3", SessionID: "s", RequestChatML: "sibling prompt", Status: "completed"},
		{ID: 4, UserID: 10, APIKeyID: 20, LogicalKey: "retry", SessionID: "s", RequestChatML: "retry prompt", Status: "completed"},
		{ID: 5, UserID: 11, APIKeyID: 20, LogicalKey: "foreign", SessionID: "s", PreviousResponseID: "r1", RequestChatML: "foreign-owner", Status: "completed"},
	}
	for _, i := range []int{2, 4, 1, 3, 0} {
		rows[i].CreatedAt = start.Add(time.Duration(i) * time.Minute)
		if err := r.Create(ctx, rows[i]); err != nil {
			t.Fatal(err)
		}
	}
	list, total, err := r.ListConversations(ctx, service.UserRequestAuditFilter{Status: "failed", Q: "needle"})
	if err != nil || total != 1 || len(list) != 1 || list[0].RequestCount != 4 {
		t.Fatalf("list=%+v total=%d err=%v", list, total, err)
	}
	if len(conversationFiles(t, r)) != 2 {
		t.Fatal("response chain not consolidated")
	}
	for _, format := range []string{"json", "jsonl"} {
		var buf bytes.Buffer
		count, err := r.ExportConversations(ctx, service.UserRequestAuditFilter{}, []int64{2}, &buf, format)
		if err != nil || count != 1 {
			t.Fatalf("count=%d err=%v", count, err)
		}
		if strings.Contains(buf.String(), "foreign-owner") {
			t.Fatal("scope leaked")
		}
		for _, text := range []string{"root prompt", "sibling prompt", "retry prompt"} {
			if !strings.Contains(buf.String(), text) {
				t.Fatalf("missing %s", text)
			}
		}
	}
}

func TestAuditConversationLargeSingleFileHasNoRequestExportCutoff(t *testing.T) {
	r := auditConversationTestRepository(t)
	rows := make([]*service.UserRequestAudit, 10001)
	now := time.Now().UTC()
	for i := range rows {
		rows[i] = &service.UserRequestAudit{ID: int64(i + 1), UserID: 1, APIKeyID: 2, SessionID: "large", CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour), RequestChatML: fmt.Sprintf("question-%d", i), Status: "failed"}
	}
	if err := auditAtomicJSON(r.conversationPath(1), r.makeDocument(rows, 1)); err != nil {
		t.Fatal(err)
	}
	reopened := NewFileUserRequestAuditRepository(nil)
	var buf bytes.Buffer
	if count, err := reopened.ExportConversations(context.Background(), service.UserRequestAuditFilter{}, nil, &buf, "jsonl"); err != nil || count != 1 {
		t.Fatalf("export %d %v", count, err)
	}
	var doc struct {
		Requests []service.UserRequestAudit `json:"requests"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Requests) != 10001 || doc.Requests[10000].RequestChatML != "question-10000" {
		t.Fatal("request cutoff")
	}
}

func TestAuditArchiveDiskGuardAndRecovery(t *testing.T) {
	r := auditConversationTestRepository(t)
	ctx := context.Background()
	r.diskUsage = func(context.Context, string) (int64, int64, error) { return 16 * userRequestAuditMinFreeBytes, 0, nil }
	if err := r.Create(ctx, &service.UserRequestAudit{LogicalKey: "blocked"}); err == nil {
		t.Fatal("low-space capture succeeded")
	}
	if len(conversationFiles(t, r)) != 0 {
		t.Fatal("low-space file persisted")
	}
	r.diskUsage = func(context.Context, string) (int64, int64, error) { return 0, 0, errors.New("probe failed") }
	if !r.StorageStatus().RecordingPaused {
		t.Fatal("failed probe not paused")
	}
	r.diskUsage = func(context.Context, string) (int64, int64, error) {
		return 16 * userRequestAuditMinFreeBytes, 8 * userRequestAuditMinFreeBytes, nil
	}
	row := &service.UserRequestAudit{LogicalKey: "resumed", RequestChatML: "evidence"}
	if err := r.Create(ctx, row); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(r.conversationPath(row.ID))
	if err != nil {
		t.Fatal(err)
	}
	r.diskUsage = func(context.Context, string) (int64, int64, error) { return 16 * userRequestAuditMinFreeBytes, 0, nil }
	if err := r.Complete(ctx, &service.UserRequestAuditCompletion{LogicalKey: "resumed", ResponseChatML: "forbidden"}); err == nil {
		t.Fatal("completion bypassed guard")
	}
	after, _ := os.ReadFile(r.conversationPath(row.ID))
	if !bytes.Equal(before, after) {
		t.Fatal("paused completion changed evidence")
	}
}

// A deployment that never captured a request must stay completely read-only:
// opening the audit page, rebooting, and re-globbing for legacy archives may
// not create the archive root, so the disk guard and "no data" state stay true.
func TestAuditArchiveBootstrapStaysReadOnlyUntilFirstCapture(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)
	root := filepath.Join(dir, "user-request-audit")
	ctx := context.Background()

	r := NewFileUserRequestAuditRepository(nil)
	r.diskUsage = func(context.Context, string) (int64, int64, error) {
		return 16 * userRequestAuditMinFreeBytes, 8 * userRequestAuditMinFreeBytes, nil
	}
	if _, total, err := r.ListConversations(ctx, service.UserRequestAuditFilter{}); err != nil || total != 0 {
		t.Fatalf("empty list %d %v", total, err)
	}
	if _, _, err := r.List(ctx, service.UserRequestAuditFilter{}); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadDir(root); !os.IsNotExist(err) {
		t.Fatalf("read-only bootstrap created archive root: %v", err)
	}

	if err := r.Create(ctx, &service.UserRequestAudit{LogicalKey: "first", RequestChatML: "evidence"}); err != nil {
		t.Fatal(err)
	}
	if len(conversationFiles(t, r)) != 1 {
		t.Fatal("first capture did not persist")
	}
	reopened := NewFileUserRequestAuditRepository(nil)
	reopened.diskUsage = r.diskUsage
	if _, total, err := reopened.ListConversations(ctx, service.UserRequestAuditFilter{}); err != nil || total != 1 {
		t.Fatalf("reopened %d %v", total, err)
	}
}

// The audit list must name the account, not just a number: username first, then
// email, and the numeric fallback only when the users table has neither.
func TestAuditConversationUserLabelPrefersAccountIdentity(t *testing.T) {
	db, err := sql.Open("sqlite", "file:audit_user_label?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err = db.Exec(`CREATE TABLE users(id INTEGER PRIMARY KEY, username TEXT, email TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO users(id,username,email) VALUES(7,'','who@example.com'),(8,'alice','a@b.c'),(9,'','')`); err != nil {
		t.Fatal(err)
	}
	r := &FileUserRequestAuditRepository{legacy: &UserRequestAuditRepository{db: db}}
	rows := []*service.UserRequestAudit{
		{UserID: 7, LogicalKey: "email"},
		{UserID: 8, LogicalKey: "username"},
		{UserID: 9, LogicalKey: "anonymous"},
		{UserID: 404, LogicalKey: "missing"},
	}
	if err := r.labelUsers(context.Background(), rows); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"who@example.com (#7)",
		"alice (#8)",
		"#9",
		"#404",
	}
	for i := range rows {
		if rows[i].UserLabel != want[i] {
			t.Fatalf("row %d label %q want %q", i, rows[i].UserLabel, want[i])
		}
	}
}

func TestAuditArchiveClearAndWholeConversationRetention(t *testing.T) {
	r := auditConversationTestRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()
	old := &service.UserRequestAudit{LogicalKey: "old", SessionID: "s", CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-time.Minute)}
	fresh := &service.UserRequestAudit{LogicalKey: "fresh", SessionID: "s", ExpiresAt: now.Add(time.Hour)}
	if err := r.Create(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := r.Create(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if n, err := r.DeleteExpired(ctx, now, 10); err != nil || n != 0 {
		t.Fatalf("partially expired session deleted %d %v", n, err)
	}
	if n, err := r.Clear(ctx); err != nil || n != 2 {
		t.Fatalf("clear %d %v", n, err)
	}
	reopened := NewFileUserRequestAuditRepository(nil)
	if _, total, err := reopened.ListConversations(ctx, service.UserRequestAuditFilter{}); err != nil || total != 0 {
		t.Fatalf("clear resurrected %d %v", total, err)
	}
	next := &service.UserRequestAudit{LogicalKey: "next"}
	if err := reopened.Create(ctx, next); err != nil {
		t.Fatal(err)
	}
	if next.ID <= fresh.ID {
		t.Fatal("clear reused ID")
	}
	if err := reopened.Complete(ctx, &service.UserRequestAuditCompletion{LogicalKey: "old", ResponseChatML: "late"}); err == nil {
		t.Fatal("cleared request resurrected")
	}
}

type auditBlockingWriter struct {
	once    sync.Once
	started chan struct{}
	release chan struct{}
	bytes.Buffer
}

func (w *auditBlockingWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started); <-w.release })
	return w.Buffer.Write(p)
}
func TestAuditExportPinsJSONWithoutBlockingCapture(t *testing.T) {
	r := auditConversationTestRepository(t)
	ctx := context.Background()
	row := &service.UserRequestAudit{LogicalKey: "before", ResponseChatML: "original"}
	if err := r.Create(ctx, row); err != nil {
		t.Fatal(err)
	}
	w := &auditBlockingWriter{started: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := r.ExportConversations(ctx, service.UserRequestAuditFilter{}, nil, w, "jsonl")
		done <- err
	}()
	<-w.started
	if err := r.Complete(ctx, &service.UserRequestAuditCompletion{LogicalKey: "before", ResponseChatML: "later"}); err != nil {
		close(w.release)
		t.Fatal(err)
	}
	close(w.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Requests []service.UserRequestAudit `json:"requests"`
	}
	if err := json.Unmarshal(w.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Requests) != 1 || doc.Requests[0].ResponseChatML != "original" {
		t.Fatal("snapshot changed")
	}
}

func TestAuditExportCancellation(t *testing.T) {
	r := auditConversationTestRepository(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.ExportConversations(ctx, service.UserRequestAuditFilter{}, nil, io.Discard, "jsonl"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel %v", err)
	}
}
