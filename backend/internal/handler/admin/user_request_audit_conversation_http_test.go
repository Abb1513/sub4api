package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/repository"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	_ "modernc.org/sqlite"
)

// Throwaway HTTP smoke: proves the wire contract the reader page depends on.
func TestScratchConversationHTTPContract(t *testing.T) {
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
		if _, err = db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	repo := repository.NewFileUserRequestAuditRepository(db)
	svc := service.NewUserRequestAuditService(repo, nil)
	handler := NewUserRequestAuditHandler(svc)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/admin/user-request-audit/:id/conversation", handler.Conversation)
	router.GET("/admin/user-request-audit/:id", handler.Get)

	ctx := context.Background()
	group := int64(4)
	history := ""
	// Use the capture format, not ambiguous plain text such as "user\n...".
	// Unframed text is intentionally one opaque message and cannot prove replay.
	var rootID int64
	for i := range 6 {
		history += fmt.Sprintf("<|im_start|>user\nquestion %d 中文历史消息 padding padding %d <|im_end|>\n", i, i)
		row := &service.UserRequestAudit{UserID: 3, APIKeyID: 9, GroupID: &group, Protocol: "openai_responses", LogicalKey: fmt.Sprintf("k%d", i), SessionID: "sess-1", RequestChatML: history, Status: "completed"}
		if err = repo.Create(ctx, row); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			rootID = row.ID
		}
		answer := fmt.Sprintf("<|im_start|>assistant\nanswer %d %d <|im_end|>\n", i, i)
		history += answer
		if err = repo.Complete(ctx, &service.UserRequestAuditCompletion{LogicalKey: row.LogicalKey, ResponseChatML: answer, Status: "completed"}); err != nil {
			t.Fatal(err)
		}
	}

	firstBody := httpGet(t, router, fmt.Sprintf("/admin/user-request-audit/%d/conversation?request_offset=0&request_limit=2", rootID))
	if strings.Contains(firstBody, "request_chatml") || strings.Contains(firstBody, "response_chatml") {
		t.Fatalf("reader response leaked raw request history copies: %s", firstBody)
	}
	var first struct {
		RequestCount int                               `json:"request_count"`
		RequestStart int                               `json:"request_start"`
		RequestEnd   int                               `json:"request_end"`
		Messages     []service.UserRequestAuditMessage `json:"messages"`
	}
	if err = json.Unmarshal([]byte(firstBody), &first); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("window 1: count=%d bounds=%d-%d messages=%d body=%d bytes\n", first.RequestCount, first.RequestStart, first.RequestEnd, len(first.Messages), len(firstBody))
	if first.RequestCount != 6 || first.RequestStart != 1 || first.RequestEnd != 2 {
		t.Fatalf("window metadata: %+v", first)
	}

	secondBody := httpGet(t, router, fmt.Sprintf("/admin/user-request-audit/%d/conversation?request_offset=2&request_limit=2", rootID))
	var second struct {
		RequestStart int                               `json:"request_start"`
		RequestEnd   int                               `json:"request_end"`
		Messages     []service.UserRequestAuditMessage `json:"messages"`
	}
	if err = json.Unmarshal([]byte(secondBody), &second); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("window 2: bounds=%d-%d messages=%d body=%d bytes\n", second.RequestStart, second.RequestEnd, len(second.Messages), len(secondBody))
	if second.RequestStart != 3 || second.RequestEnd != 4 {
		t.Fatalf("second window metadata: %+v", second)
	}
	for _, message := range second.Messages {
		if strings.Contains(message.Content, "question 0 ") || strings.Contains(message.Content, "answer 0 ") {
			t.Fatalf("window 2 repeated already-shown history: %q", message.Content)
		}
	}
	fmt.Printf("concatenated blocks over HTTP after two windows: %d\n", len(first.Messages)+len(second.Messages))

	detailBody := httpGet(t, router, fmt.Sprintf("/admin/user-request-audit/%d", rootID+3))
	if !strings.Contains(detailBody, "request_chatml") {
		t.Fatalf("detail lost its own evidence: %s", detailBody)
	}
	if strings.Contains(detailBody, "\"chatml\"") {
		t.Fatalf("detail still ships a rebuilt conversation transcript: %d bytes", len(detailBody))
	}
	fmt.Printf("detail body=%d bytes: own chatml kept, merged transcript removed\n", len(detailBody))
}

// httpGet mirrors the frontend axios interceptor: assert HTTP 200 and return
// the unwrapped `data` member of {code,message,data}.
func httpGet(t *testing.T, router http.Handler, path string) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s -> %d %s", path, recorder.Code, recorder.Body.String())
	}
	var envelope struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("%s envelope: %v body=%s", path, err, recorder.Body.String())
	}
	if envelope.Code != 0 {
		t.Fatalf("%s envelope code=%d message=%s", path, envelope.Code, envelope.Message)
	}
	return string(envelope.Data)
}
