package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/shirou/gopsutil/v4/disk"
)

const userRequestAuditMinFreeBytes int64 = 2 * 1024 * 1024 * 1024

type auditArchiveEntry struct {
	row            service.UserRequestAudit
	conversationID int64
	proof          service.UserRequestAuditContinuityProof
}
type auditDocumentMeta struct {
	summary service.UserRequestAudit
	path    string
	expires time.Time
}
type auditDiskConversation struct {
	Version int `json:"version"`
	*service.UserRequestAuditConversation
	LogicalKeys map[string]string `json:"logical_keys"`
}
type auditArchiveState struct {
	NextID      int64   `json:"next_id"`
	Migrated    bool    `json:"legacy_import_complete"`
	DeleteIDs   []int64 `json:"pending_delete_ids,omitempty"`
	Clear       bool    `json:"pending_clear,omitempty"`
	PendingIDs  []int64 `json:"pending_document_ids,omitempty"`
	ObsoleteIDs []int64 `json:"obsolete_document_ids,omitempty"`
}

// Payloads are authoritative atomic JSON documents. SQL contains only metadata
// and relative paths; the in-memory maps support capture association, not polling.
type FileUserRequestAuditRepository struct {
	root         string
	legacy       *UserRequestAuditRepository
	mu           sync.Mutex
	lastCleanup  time.Time
	state        auditArchiveState
	index        map[int64]auditArchiveEntry
	documents    map[int64]auditDocumentMeta
	logicalIndex map[string]int64
	diskUsage    func(context.Context, string) (int64, int64, error)
}

func NewFileUserRequestAuditRepository(db *sql.DB) *FileUserRequestAuditRepository {
	root := strings.TrimSpace(os.Getenv("DATA_DIR"))
	if root == "" {
		root = "./data"
	}
	r := &FileUserRequestAuditRepository{root: filepath.Join(root, "user-request-audit")}
	if db != nil {
		r.legacy = NewUserRequestAuditRepository(db)
	}
	// The Ent provider completes SQL migrations before exposing this DB. Any
	// recovery error remains fail-closed and is retried by the first operation.
	_ = r.ensureIndexLocked(context.Background())
	return r
}

func (r *FileUserRequestAuditRepository) conversationPath(id int64) string {
	return filepath.Join(r.root, "conversations", strconv.FormatInt(id, 10)+".json")
}
func (r *FileUserRequestAuditRepository) statePath() string {
	return filepath.Join(r.root, "archive-state.json")
}
func auditAtomicJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".audit-*.tmp")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0o640); err == nil {
		err = json.NewEncoder(f).Encode(value)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	return auditSyncDirectory(filepath.Dir(path))
}
func auditSyncDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (r *FileUserRequestAuditRepository) saveStateLocked() error {
	return auditAtomicJSON(r.statePath(), r.state)
}
func readAuditConversation(path string) (*auditDiskConversation, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var doc auditDiskConversation
	if err = json.NewDecoder(f).Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode audit conversation %s: %w", filepath.Base(path), err)
	}
	if doc.Version != 1 || doc.UserRequestAuditConversation == nil || doc.ID <= 0 || len(doc.Requests) == 0 {
		return nil, fmt.Errorf("invalid audit conversation %s", filepath.Base(path))
	}
	seen := make(map[int64]bool, len(doc.Requests))
	for _, row := range doc.Requests {
		if row == nil || row.ID <= 0 || seen[row.ID] || row.UserID != doc.Requests[0].UserID || row.APIKeyID != doc.Requests[0].APIKeyID {
			return nil, fmt.Errorf("invalid audit request membership %s", filepath.Base(path))
		}
		seen[row.ID] = true
		row.LogicalKey = doc.LogicalKeys[strconv.FormatInt(row.ID, 10)]
	}
	if !seen[doc.ID] {
		return nil, fmt.Errorf("missing audit root %d", doc.ID)
	}
	return &doc, nil
}
func auditMetadata(row *service.UserRequestAudit) service.UserRequestAudit {
	result := *row
	result.RequestChatML = ""
	result.ResponseChatML = ""
	result.ChatML = ""
	result.RequestCount = 0
	result.UserLabel = ""
	return result
}
func (r *FileUserRequestAuditRepository) indexDocumentLocked(doc *auditDiskConversation) {
	rows := doc.Requests
	summary := auditConversationSummary(auditConversation{rows: rows, representative: rows[0], latest: rows[len(rows)-1]})
	summary.ID = doc.ID
	expires := rows[0].ExpiresAt
	for _, row := range rows {
		if row.ExpiresAt.After(expires) {
			expires = row.ExpiresAt
		}
		if row.ID > r.state.NextID {
			r.state.NextID = row.ID
		}
		meta := auditMetadata(row)
		r.index[row.ID] = auditArchiveEntry{row: meta, conversationID: doc.ID, proof: service.UserRequestAuditProof(row)}
		if row.LogicalKey != "" {
			r.logicalIndex[row.LogicalKey] = row.ID
		}
	}
	r.documents[doc.ID] = auditDocumentMeta{summary: *summary, path: r.conversationPath(doc.ID), expires: expires}
}
func (r *FileUserRequestAuditRepository) ensureIndexLocked(ctx context.Context) (err error) {
	if r.index != nil {
		return ctx.Err()
	}
	defer func() {
		if err != nil {
			r.index = nil
			r.documents = nil
			r.logicalIndex = nil
		}
	}()
	if err = os.MkdirAll(filepath.Join(r.root, "conversations"), 0o750); err != nil {
		return err
	}
	if b, e := os.ReadFile(r.statePath()); e == nil {
		if err = json.Unmarshal(b, &r.state); err != nil {
			return err
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	if err = r.finishWritesLocked(ctx); err != nil {
		return err
	}
	if err = os.RemoveAll(filepath.Join(r.root, "pending")); err != nil {
		return err
	}
	if err = r.finishDeletesLocked(ctx); err != nil {
		return err
	}
	r.index = make(map[int64]auditArchiveEntry)
	r.documents = make(map[int64]auditDocumentMeta)
	r.logicalIndex = make(map[string]int64)
	files, err := filepath.Glob(filepath.Join(r.root, "conversations", "*.json"))
	if err != nil {
		return err
	}
	// Overlapping files can only be left by an interrupted consolidation. Fold
	// latest rows, then rewrite the proven groups before publishing the index.
	latest := make(map[int64]*service.UserRequestAudit)
	overlap := false
	for _, path := range files {
		if err = ctx.Err(); err != nil {
			return err
		}
		doc, e := readAuditConversation(path)
		if e != nil {
			return e
		}
		if filepath.Clean(path) != filepath.Clean(r.conversationPath(doc.ID)) {
			return fmt.Errorf("audit file identity mismatch")
		}
		for _, row := range doc.Requests {
			if old := latest[row.ID]; old != nil {
				overlap = true
				if old.UpdatedAt.After(row.UpdatedAt) {
					continue
				}
			}
			latest[row.ID] = row
		}
		r.indexDocumentLocked(doc)
	}
	if !r.state.Migrated {
		if err = r.importLegacyLocked(ctx, latest); err != nil {
			return err
		}
	} else if overlap {
		rows := make([]*service.UserRequestAudit, 0, len(latest))
		for _, row := range latest {
			rows = append(rows, row)
		}
		if err = r.replaceAllDocumentsLocked(ctx, rows); err != nil {
			return err
		}
	}
	if err = r.reconcileSQLLocked(ctx); err != nil {
		return err
	}
	return r.saveStateLocked()
}
func (r *FileUserRequestAuditRepository) makeDocument(rows []*service.UserRequestAudit, id int64) *auditDiskConversation {
	sort.Slice(rows, func(i, j int) bool { return auditRowBefore(rows[i], rows[j]) })
	keys := make(map[string]string, len(rows))
	for _, row := range rows {
		keys[strconv.FormatInt(row.ID, 10)] = row.LogicalKey
	}
	return &auditDiskConversation{Version: 1, UserRequestAuditConversation: &service.UserRequestAuditConversation{ID: id, UserLabel: "#" + strconv.FormatInt(rows[0].UserID, 10), RequestCount: len(rows), Messages: service.BuildUserRequestAuditTranscript(rows), Requests: rows}, LogicalKeys: keys}
}
func (r *FileUserRequestAuditRepository) persistRowLocked(ctx context.Context, row *service.UserRequestAudit) (err error) {
	defer func() {
		if err != nil {
			r.index = nil
		}
	}()
	metas := make([]*service.UserRequestAudit, 0, len(r.index)+1)
	proofs := make(map[int64]service.UserRequestAuditContinuityProof, len(r.index)+1)
	for id, entry := range r.index {
		if id == row.ID {
			continue
		}
		copy := entry.row
		metas = append(metas, &copy)
		proofs[id] = entry.proof
	}
	meta := auditMetadata(row)
	metas = append(metas, &meta)
	proofs[row.ID] = service.UserRequestAuditProof(row)
	groups := groupAuditConversationsWithProof(metas, proofs)
	loaded := make(map[int64]*auditDiskConversation)
	changed := make([]*auditDiskConversation, 0)
	keep := make(map[int64]bool)
	for _, group := range groups {
		id := group.representative.ID
		keep[id] = true
		needs := false
		for _, m := range group.rows {
			old, ok := r.index[m.ID]
			needs = needs || m.ID == row.ID || !ok || old.conversationID != id
		}
		if existing, ok := r.documents[id]; !ok || existing.summary.RequestCount != len(group.rows) {
			needs = true
		}
		if !needs {
			continue
		}
		full := make([]*service.UserRequestAudit, 0, len(group.rows))
		for _, m := range group.rows {
			if m.ID == row.ID {
				full = append(full, row)
				continue
			}
			entry := r.index[m.ID]
			doc := loaded[entry.conversationID]
			if doc == nil {
				var e error
				doc, e = readAuditConversation(r.conversationPath(entry.conversationID))
				if e != nil {
					return e
				}
				loaded[entry.conversationID] = doc
			}
			found := false
			for _, prior := range doc.Requests {
				if prior.ID == m.ID {
					full = append(full, prior)
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("audit request %d missing from file", m.ID)
			}
		}
		changed = append(changed, r.makeDocument(full, id))
	}
	obsolete := make([]int64, 0)
	for id := range r.documents {
		if !keep[id] {
			obsolete = append(obsolete, id)
		}
	}
	if err = r.commitDocumentsLocked(ctx, changed, obsolete); err != nil {
		return err
	}
	for _, id := range obsolete {
		delete(r.documents, id)
	}
	for _, doc := range changed {
		r.indexDocumentLocked(doc)
	}
	return r.syncSQLDocumentsLocked(ctx, changed, obsolete, false)
}
func (r *FileUserRequestAuditRepository) Create(ctx context.Context, row *service.UserRequestAudit) error {
	if row == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureIndexLocked(ctx); err != nil {
		return err
	}
	now := time.Now().UTC()
	if row.CreatedAt.IsZero() {
		row.CreatedAt = now
	}
	if row.UpdatedAt.IsZero() {
		row.UpdatedAt = row.CreatedAt
	}
	if row.ExpiresAt.IsZero() {
		row.ExpiresAt = row.CreatedAt.AddDate(0, 0, service.UserRequestAuditDefaultRetentionDays)
	}
	if row.ID <= 0 {
		r.state.NextID++
		row.ID = r.state.NextID
	}
	if row.ID > r.state.NextID {
		r.state.NextID = row.ID
	}
	if err := r.saveStateLocked(); err != nil {
		return err
	}
	if row.ConversationKey == "" {
		if row.SessionID != "" {
			row.ConversationKey = "session:" + row.SessionID
		} else if row.ResponseID != "" {
			row.ConversationKey = "response:" + row.ResponseID
		} else {
			row.ConversationKey = "request:" + strconv.FormatInt(row.ID, 10)
		}
	}
	return r.persistRowLocked(ctx, row)
}
func (r *FileUserRequestAuditRepository) Complete(ctx context.Context, c *service.UserRequestAuditCompletion) error {
	if c == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureIndexLocked(ctx); err != nil {
		return err
	}
	id, ok := r.logicalIndex[c.LogicalKey]
	if !ok {
		return sql.ErrNoRows
	}
	entry := r.index[id]
	doc, err := readAuditConversation(r.conversationPath(entry.conversationID))
	if err != nil {
		return err
	}
	var row *service.UserRequestAudit
	for _, candidate := range doc.Requests {
		if candidate.ID == id {
			row = candidate
			break
		}
	}
	if row == nil {
		return sql.ErrNoRows
	}
	row.UpdatedAt = time.Now().UTC()
	if c.ResponseID != "" {
		row.ResponseID = c.ResponseID
		normalizeConversationKey(row)
	}
	if c.UpstreamModel != "" {
		row.UpstreamModel = c.UpstreamModel
	}
	if c.ResponseChatML != "" {
		row.ResponseChatML = appendAuditChatMLFile(row.ResponseChatML, c.ResponseChatML)
	}
	if c.Status != "" {
		row.Status = c.Status
	}
	if c.InputUsage != nil {
		row.InputUsage = c.InputUsage
	}
	if c.OutputUsage != nil {
		row.OutputUsage = c.OutputUsage
	}
	if c.CacheUsage != nil {
		row.CacheUsage = c.CacheUsage
	}
	if c.LastError != "" {
		row.LastError = c.LastError
	}
	if len(c.Metadata) > 0 {
		if row.Metadata == nil {
			row.Metadata = make(map[string]any)
		}
		for key, value := range c.Metadata {
			row.Metadata[key] = value
		}
	}
	return r.persistRowLocked(ctx, row)
}
func (r *FileUserRequestAuditRepository) GetConversation(ctx context.Context, id int64) (*service.UserRequestAuditConversation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureIndexLocked(ctx); err != nil {
		return nil, err
	}
	entry, ok := r.index[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	doc, err := readAuditConversation(r.conversationPath(entry.conversationID))
	if err != nil {
		return nil, err
	}
	if err = r.labelUsers(ctx, doc.Requests); err != nil {
		return nil, err
	}
	doc.UserLabel = doc.Requests[0].UserLabel
	return doc.UserRequestAuditConversation, nil
}
func (r *FileUserRequestAuditRepository) GetByID(ctx context.Context, id int64) (*service.UserRequestAudit, error) {
	conversation, err := r.GetConversation(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, row := range conversation.Requests {
		if row.ID == id {
			normalizeConversationKey(row)
			row.ChatML = auditMessagesTranscript(conversation.Messages)
			row.RequestCount = conversation.RequestCount
			return row, nil
		}
	}
	return nil, sql.ErrNoRows
}
func (r *FileUserRequestAuditRepository) finishDeletesLocked(ctx context.Context) error {
	ids := r.state.DeleteIDs
	if r.state.Clear {
		files, err := filepath.Glob(filepath.Join(r.root, "conversations", "*.json"))
		if err != nil {
			return err
		}
		for _, path := range files {
			if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	} else {
		for _, id := range ids {
			if err := os.Remove(r.conversationPath(id)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	if r.state.Clear {
		if err := os.RemoveAll(r.root + "-migration-backup"); err != nil {
			return err
		}
		if err := auditSyncDirectory(filepath.Dir(r.root)); err != nil {
			return err
		}
		if err := os.RemoveAll(filepath.Join(r.root, "pending")); err != nil {
			return err
		}
	}
	if r.state.Clear || len(ids) > 0 {
		if err := auditSyncDirectory(filepath.Join(r.root, "conversations")); err != nil {
			return err
		}
	}
	if r.legacy != nil && (r.state.Clear || len(ids) > 0) {
		if r.state.Clear {
			if _, err := r.legacy.db.ExecContext(ctx, "DELETE FROM user_request_audit_conversations"); err != nil {
				return err
			}
		} else {
			for _, id := range ids {
				if _, err := r.legacy.db.ExecContext(ctx, "DELETE FROM user_request_audit_conversations WHERE id=$1", id); err != nil {
					return err
				}
			}
		}
	}
	if r.state.Clear || len(ids) > 0 {
		r.state.Clear = false
		r.state.DeleteIDs = nil
		return r.saveStateLocked()
	}
	return nil
}
func (r *FileUserRequestAuditRepository) Clear(ctx context.Context) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureIndexLocked(ctx); err != nil {
		return 0, err
	}
	count := int64(len(r.index))
	r.state.Migrated = true
	r.state.Clear = true
	r.index = nil
	r.documents = nil
	r.logicalIndex = nil
	if err := r.saveStateLocked(); err != nil {
		return 0, err
	}
	if err := r.finishDeletesLocked(ctx); err != nil {
		return 0, err
	}
	return count, nil
}
func (r *FileUserRequestAuditRepository) DeleteExpired(ctx context.Context, before time.Time, batchSize int) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureIndexLocked(ctx); err != nil {
		return 0, err
	}
	if batchSize <= 0 {
		batchSize = 500
	}
	var count int64
	for id, doc := range r.documents {
		if doc.expires.Before(before) && len(r.state.DeleteIDs) < batchSize {
			r.state.DeleteIDs = append(r.state.DeleteIDs, id)
			count += int64(doc.summary.RequestCount)
		}
	}
	if len(r.state.DeleteIDs) == 0 {
		return 0, nil
	}
	r.index = nil
	if err := r.saveStateLocked(); err != nil {
		return 0, err
	}
	if err := r.finishDeletesLocked(ctx); err != nil {
		return 0, err
	}
	r.lastCleanup = time.Now().UTC()
	return count, nil
}
func normalizeConversationKey(row *service.UserRequestAudit) {
	if row != nil && row.ResponseID != "" && (strings.HasPrefix(row.ConversationKey, "fallback:") || strings.HasPrefix(row.ConversationKey, "request:")) {
		row.ConversationKey = "response:" + row.ResponseID
	}
}
func sameGroup(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
func appendAuditChatMLFile(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" || a == b {
		return a
	}
	return a + "\n" + b
}
func (r *FileUserRequestAuditRepository) UserRequestAuditRoot() string {
	if r == nil {
		return ""
	}
	return r.root
}

var _ service.UserRequestAuditRepository = (*FileUserRequestAuditRepository)(nil)

func (r *FileUserRequestAuditRepository) StorageStatus() service.UserRequestAuditStorageStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := service.UserRequestAuditStorageStatus{LastCleanup: r.lastCleanup}
	out.DiskTotalBytes, out.DiskFreeBytes, out.PauseReason = r.diskStatusLocked(context.Background())
	out.RecordingPaused = out.PauseReason != ""
	files, _ := filepath.Glob(filepath.Join(r.root, "conversations", "*.json"))
	out.FileCount = len(files)
	for _, path := range files {
		st, err := os.Stat(path)
		if err != nil {
			continue
		}
		out.TotalBytes += st.Size()
		if st.ModTime().After(out.Latest) {
			out.Latest = st.ModTime()
		}
		if out.Oldest.IsZero() || st.ModTime().Before(out.Oldest) {
			out.Oldest = st.ModTime()
		}
	}
	return out
}
func (r *FileUserRequestAuditRepository) configPath() string {
	return filepath.Join(r.root, "config.json")
}
func (r *FileUserRequestAuditRepository) readConfig() (service.UserRequestAuditConfig, bool) {
	var cfg service.UserRequestAuditConfig
	b, err := os.ReadFile(r.configPath())
	if err != nil || json.Unmarshal(b, &cfg) != nil {
		return cfg, false
	}
	return cfg, true
}
func (r *FileUserRequestAuditRepository) GetAuditConfig() service.UserRequestAuditConfig {
	if cfg, ok := r.readConfig(); ok {
		return cfg
	}
	return service.UserRequestAuditConfig{CleanupIntervalHours: service.UserRequestAuditDefaultCleanupIntervalHours}
}
func (r *FileUserRequestAuditRepository) SetAuditConfig(cfg service.UserRequestAuditConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return auditAtomicJSON(r.configPath(), cfg)
}

func (r *FileUserRequestAuditRepository) diskStatusLocked(ctx context.Context) (int64, int64, string) {
	path, err := filepath.Abs(r.root)
	if err != nil {
		return 0, 0, "disk space unavailable"
	}
	for {
		_, err = os.Stat(path)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) || filepath.Dir(path) == path {
			return 0, 0, "disk space unavailable"
		}
		path = filepath.Dir(path)
	}
	var total, free int64
	if r.diskUsage != nil {
		total, free, err = r.diskUsage(ctx, path)
	} else {
		var usage *disk.UsageStat
		usage, err = disk.UsageWithContext(ctx, path)
		if err == nil && usage != nil {
			total, free = int64(usage.Total), int64(usage.Free)
		}
	}
	if err != nil || total <= 0 || free < 0 {
		return 0, 0, "disk space unavailable"
	}
	if free < userRequestAuditMinFreeBytes {
		return total, free, "less than 2 GiB free disk space"
	}
	return total, free, ""
}
func (r *FileUserRequestAuditRepository) checkDiskLocked(ctx context.Context) error {
	_, _, reason := r.diskStatusLocked(ctx)
	if reason != "" {
		return fmt.Errorf("audit recording paused: %s", reason)
	}
	return nil
}
