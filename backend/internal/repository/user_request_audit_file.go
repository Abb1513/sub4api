package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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

// Payloads stay authoritative atomic JSON documents; SQL carries only metadata
// and relative paths, and the in-memory maps support capture association.
//
// Locking contract for this file family:
//
//	mu    guards r.index, r.documents, r.logicalIndex, r.staleDocs and r.state.
//	      Sections are map/state operations only - never file or SQL I/O.
//	ioMu  serializes archive mutations (index build, publication, clear,
//	      expiry). Read paths never take ioMu.
//
// A writer therefore cannot block a list request or a conversation read. A
// failed publication degrades only the documents it touched: they stay marked
// stale and are re-derived from their own files. It never drops the whole
// index, because a full rescan inside the bounded writer deadline used to fail
// again and starve every later request behind the shared lock.
type FileUserRequestAuditRepository struct {
	root         string
	legacy       *UserRequestAuditRepository
	mu           sync.Mutex
	ioMu         sync.Mutex
	lastCleanup  time.Time
	state        auditArchiveState
	index        map[int64]auditArchiveEntry
	documents    map[int64]auditDocumentMeta
	logicalIndex map[string]int64
	staleDocs    map[int64]bool
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
	_ = r.ensureIndex(context.Background())
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

// saveStateLocked persists the journal. Callers hold mu. The journal holds ID
// lists only, never payload bytes.
func (r *FileUserRequestAuditRepository) saveStateLocked() error {
	return auditAtomicJSON(r.statePath(), r.state)
}

// saveState persists the journal, taking mu.
func (r *FileUserRequestAuditRepository) saveState() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.saveStateLocked()
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

// indexDocumentLocked installs one document into the association maps. Callers
// hold mu.
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
	delete(r.staleDocs, doc.ID)
}

// forgetDocumentLocked drops one document and its request associations. Callers
// hold mu.
func (r *FileUserRequestAuditRepository) forgetDocumentLocked(id int64) {
	for rowID, entry := range r.index {
		if entry.conversationID != id {
			continue
		}
		if entry.row.LogicalKey != "" && r.logicalIndex[entry.row.LogicalKey] == rowID {
			delete(r.logicalIndex, entry.row.LogicalKey)
		}
		delete(r.index, rowID)
	}
	delete(r.documents, id)
	delete(r.staleDocs, id)
}

// resetMapsLocked publishes empty association maps. Callers hold mu.
func (r *FileUserRequestAuditRepository) resetMapsLocked() {
	r.index = make(map[int64]auditArchiveEntry)
	r.documents = make(map[int64]auditDocumentMeta)
	r.logicalIndex = make(map[string]int64)
	r.staleDocs = make(map[int64]bool)
}

// indexReady reports whether the association maps are published.
func (r *FileUserRequestAuditRepository) indexReady() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.index != nil
}

// ensureIndex prepares the association maps. The archive scan runs without mu,
// so a first request never blocks other readers, and it is idempotent.
func (r *FileUserRequestAuditRepository) ensureIndex(ctx context.Context) error {
	r.mu.Lock()
	built := r.index != nil
	r.mu.Unlock()
	if built {
		return ctx.Err()
	}
	r.ioMu.Lock()
	defer r.ioMu.Unlock()
	return r.buildIndex(ctx)
}

// invalidateDocuments marks documents whose on-disk state could not be read or
// published. Only those are re-derived later; the rest of the index keeps
// serving reads and future captures.
func (r *FileUserRequestAuditRepository) invalidateDocuments(ids ...int64) {
	if len(ids) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.staleDocs == nil {
		r.staleDocs = make(map[int64]bool, len(ids))
	}
	for _, id := range ids {
		r.staleDocs[id] = true
	}
}

// repairStaleDocuments re-reads only documents a failed publication touched, so
// recovery cost is proportional to the failure rather than archive size.
func (r *FileUserRequestAuditRepository) repairStaleDocuments(ctx context.Context) error {
	for {
		r.mu.Lock()
		var id int64
		for candidate := range r.staleDocs {
			id = candidate
			break
		}
		if id == 0 {
			r.mu.Unlock()
			return ctx.Err()
		}
		r.mu.Unlock()
		doc, err := r.loadAuditDocument(ctx, id)
		r.mu.Lock()
		if err != nil {
			// The document vanished or is unreadable. Forget it so capture stops
			// retrying a broken file; the durable journal still records the loss.
			delete(r.staleDocs, id)
			r.forgetDocumentLocked(id)
		} else {
			r.forgetDocumentLocked(id)
			r.indexDocumentLocked(doc)
		}
		r.mu.Unlock()
	}
}

// buildIndex performs the full archive scan. Callers hold ioMu. Bootstrap is
// all-or-nothing: an unreadable archive leaves no published index, so reads
// keep failing closed instead of silently serving half-imported evidence.
func (r *FileUserRequestAuditRepository) buildIndex(ctx context.Context) (err error) {
	r.mu.Lock()
	if r.index != nil {
		r.mu.Unlock()
		return ctx.Err()
	}
	r.mu.Unlock()
	defer func() {
		if err != nil {
			r.mu.Lock()
			r.resetMapsLocked()
			r.index = nil
			r.documents = nil
			r.logicalIndex = nil
			r.staleDocs = nil
			r.mu.Unlock()
		}
	}()

	var state auditArchiveState
	if b, e := os.ReadFile(r.statePath()); e == nil {
		if err = json.Unmarshal(b, &state); err != nil {
			return err
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	r.mu.Lock()
	r.state = state
	r.resetMapsLocked()
	r.mu.Unlock()

	if err = r.finishWrites(ctx); err != nil {
		return err
	}
	if err = os.RemoveAll(filepath.Join(r.root, "pending")); err != nil {
		return err
	}
	if err = r.finishDeletes(ctx); err != nil {
		return err
	}

	files, err := filepath.Glob(filepath.Join(r.root, "conversations", "*.json"))
	if err != nil {
		return err
	}
	// Overlapping files can only be left by an interrupted consolidation. Fold
	// the latest rows, then rewrite the proven groups before publishing.
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
		r.mu.Lock()
		r.indexDocumentLocked(doc)
		r.mu.Unlock()
	}
	// Bootstrap must stay read-only unless it actually repaired something:
	// creating the archive root for a deployment that never captured anything
	// would be a write driven purely by someone opening the page.
	persisted := false
	if !r.state.Migrated {
		persisted, err = r.importLegacy(ctx, latest)
		if err != nil {
			return err
		}
	} else if overlap {
		rows := make([]*service.UserRequestAudit, 0, len(latest))
		for _, row := range latest {
			rows = append(rows, row)
		}
		if err = r.replaceAllDocuments(ctx, rows); err != nil {
			return err
		}
		persisted = true
	}
	r.mu.Lock()
	reconcile := r.legacy != nil
	r.mu.Unlock()
	if reconcile {
		if err = r.reconcileSQL(ctx); err != nil {
			return err
		}
	}
	if !persisted {
		return ctx.Err()
	}
	return r.saveState()
}

// loadAuditDocument reads one conversation payload without any repository lock.
// Publications replace files by atomic rename, so this always observes either
// the previous or the new complete document.
func (r *FileUserRequestAuditRepository) loadAuditDocument(ctx context.Context, id int64) (*auditDiskConversation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return readAuditConversation(r.conversationPath(id))
}

func (r *FileUserRequestAuditRepository) makeDocument(rows []*service.UserRequestAudit, id int64) *auditDiskConversation {
	sort.Slice(rows, func(i, j int) bool { return auditRowBefore(rows[i], rows[j]) })
	keys := make(map[string]string, len(rows))
	for _, row := range rows {
		keys[strconv.FormatInt(row.ID, 10)] = row.LogicalKey
	}
	return &auditDiskConversation{Version: 1, UserRequestAuditConversation: &service.UserRequestAuditConversation{ID: id, UserLabel: "#" + strconv.FormatInt(rows[0].UserID, 10), RequestCount: len(rows), Messages: service.BuildUserRequestAuditTranscript(rows), Requests: rows}, LogicalKeys: keys}
}

// persistRow publishes only the conversations a single row can affect. It never
// regroups the whole archive and never holds mu across payload encoding.
func (r *FileUserRequestAuditRepository) persistRow(ctx context.Context, row *service.UserRequestAudit) error {
	r.ioMu.Lock()
	defer r.ioMu.Unlock()
	if err := r.repairStaleDocuments(ctx); err != nil {
		return err
	}
	if err := r.ensureIndex(ctx); err != nil {
		return err
	}

	// Only the owner/API-key scope this row can merge with takes part in
	// regrouping. That scope is one caller's captures, not the whole archive.
	userID, apiKeyID := row.UserID, row.APIKeyID
	metas := make([]*service.UserRequestAudit, 0, 16)
	proofs := make(map[int64]service.UserRequestAuditContinuityProof, 16)
	r.mu.Lock()
	if r.index == nil {
		r.mu.Unlock()
		return errors.New("audit archive index is unavailable")
	}
	for id, entry := range r.index {
		if id == row.ID || entry.row.UserID != userID || entry.row.APIKeyID != apiKeyID {
			continue
		}
		if r.staleDocs[entry.conversationID] {
			stale := entry.conversationID
			r.mu.Unlock()
			return fmt.Errorf("audit archive is recovering conversation %d", stale)
		}
		metadata := entry.row
		metas = append(metas, &metadata)
		proofs[id] = entry.proof
	}
	r.mu.Unlock()

	summary := auditMetadata(row)
	metas = append(metas, &summary)
	proofs[row.ID] = service.UserRequestAuditProof(row)
	groups := groupAuditConversationsWithProof(metas, proofs)

	loaded := make(map[int64]*auditDiskConversation)
	changed := make([]*auditDiskConversation, 0)
	keep := make(map[int64]bool, len(groups))
	for _, group := range groups {
		id := group.representative.ID
		keep[id] = true
		needs := false
		r.mu.Lock()
		for _, m := range group.rows {
			old, ok := r.index[m.ID]
			needs = needs || m.ID == row.ID || !ok || old.conversationID != id
		}
		if existing, ok := r.documents[id]; !ok || existing.summary.RequestCount != len(group.rows) {
			needs = true
		}
		r.mu.Unlock()
		if !needs {
			continue
		}
		full := make([]*service.UserRequestAudit, 0, len(group.rows))
		for _, m := range group.rows {
			if m.ID == row.ID {
				full = append(full, row)
				continue
			}
			r.mu.Lock()
			conversationID := r.index[m.ID].conversationID
			r.mu.Unlock()
			doc := loaded[conversationID]
			if doc == nil {
				var e error
				doc, e = r.loadAuditDocument(ctx, conversationID)
				if e != nil {
					r.invalidateDocuments(conversationID)
					return e
				}
				loaded[conversationID] = doc
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
				r.invalidateDocuments(conversationID)
				return fmt.Errorf("audit request %d missing from file", m.ID)
			}
		}
		changed = append(changed, r.makeDocument(full, id))
	}

	// Retire only documents inside this scope that the recompute dropped; other
	// documents keep their identity regardless of what this row did.
	obsolete := make([]int64, 0)
	r.mu.Lock()
	for id, meta := range r.documents {
		if keep[id] || meta.summary.UserID != userID || meta.summary.APIKeyID != apiKeyID {
			continue
		}
		obsolete = append(obsolete, id)
	}
	r.mu.Unlock()

	if err := r.commitDocuments(ctx, changed, obsolete); err != nil {
		return err
	}
	if err := r.syncSQLDocuments(ctx, changed, obsolete, false); err != nil {
		// Files are authoritative. A metadata-projection failure must never lose
		// a capture; the next full index build reconciles SQL again.
		slog.Warn("user_request_audit.sql_sync_failed", "error", err)
	}
	return nil
}

func (r *FileUserRequestAuditRepository) Create(ctx context.Context, row *service.UserRequestAudit) error {
	if row == nil {
		return nil
	}
	if err := r.ensureIndex(ctx); err != nil {
		return err
	}
	if err := r.repairStaleDocuments(ctx); err != nil {
		return err
	}
	r.mu.Lock()
	if r.index == nil {
		r.mu.Unlock()
		return errors.New("audit archive index is unavailable")
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
		r.mu.Unlock()
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
	r.mu.Unlock()
	return r.persistRow(ctx, row)
}

func (r *FileUserRequestAuditRepository) Complete(ctx context.Context, c *service.UserRequestAuditCompletion) error {
	if c == nil {
		return nil
	}
	if err := r.ensureIndex(ctx); err != nil {
		return err
	}
	if err := r.repairStaleDocuments(ctx); err != nil {
		return err
	}
	r.mu.Lock()
	entry, ok := r.index[r.logicalIndex[c.LogicalKey]]
	r.mu.Unlock()
	if !ok {
		return sql.ErrNoRows
	}
	doc, err := r.loadAuditDocument(ctx, entry.conversationID)
	if err != nil {
		r.invalidateDocuments(entry.conversationID)
		return err
	}
	var row *service.UserRequestAudit
	for _, candidate := range doc.Requests {
		if candidate.ID == entry.row.ID {
			row = candidate
			break
		}
	}
	if row == nil {
		r.invalidateDocuments(entry.conversationID)
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
	return r.persistRow(ctx, row)
}

// resolveConversationFile resolves the payload path for a request or
// conversation id, waiting only for the index and never for a payload rewrite.
func (r *FileUserRequestAuditRepository) resolveConversationFile(ctx context.Context, id int64) (string, int64, error) {
	if err := r.ensureIndex(ctx); err != nil {
		return "", 0, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.index[id]
	if !ok {
		return "", 0, sql.ErrNoRows
	}
	if r.staleDocs[entry.conversationID] {
		return "", 0, fmt.Errorf("audit conversation %d is being recovered", entry.conversationID)
	}
	return r.conversationPath(entry.conversationID), entry.conversationID, nil
}

func (r *FileUserRequestAuditRepository) GetConversation(ctx context.Context, id int64) (*service.UserRequestAuditConversation, error) {
	path, _, err := r.resolveConversationFile(ctx, id)
	if err != nil {
		return nil, err
	}
	doc, err := readAuditConversation(path)
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
	path, _, err := r.resolveConversationFile(ctx, id)
	if err != nil {
		return nil, err
	}
	doc, err := readAuditConversation(path)
	if err != nil {
		return nil, err
	}
	for _, row := range doc.Requests {
		if row.ID == id {
			normalizeConversationKey(row)
			row.ChatML = auditMessagesTranscript(doc.Messages)
			row.RequestCount = doc.RequestCount
			if err = r.labelUsers(ctx, []*service.UserRequestAudit{row}); err != nil {
				return nil, err
			}
			return row, nil
		}
	}
	return nil, sql.ErrNoRows
}

func (r *FileUserRequestAuditRepository) Clear(ctx context.Context) (int64, error) {
	r.ioMu.Lock()
	defer r.ioMu.Unlock()
	if err := r.ensureIndex(ctx); err != nil {
		return 0, err
	}
	r.mu.Lock()
	count := int64(len(r.index))
	r.state.Migrated = true
	r.state.Clear = true
	r.state.PendingIDs = nil
	r.state.ObsoleteIDs = nil
	r.resetMapsLocked()
	// Empty maps stay published so readers keep serving (empty) results during
	// the file sweep instead of triggering another archive scan.
	if err := r.saveStateLocked(); err != nil {
		r.mu.Unlock()
		return 0, err
	}
	r.mu.Unlock()
	if err := r.finishDeletes(ctx); err != nil {
		return 0, err
	}
	if r.legacy != nil {
		if err := r.syncSQLDocuments(ctx, nil, nil, true); err != nil {
			return 0, err
		}
	}
	return count, nil
}

func (r *FileUserRequestAuditRepository) DeleteExpired(ctx context.Context, before time.Time, batchSize int) (int64, error) {
	r.ioMu.Lock()
	defer r.ioMu.Unlock()
	if err := r.ensureIndex(ctx); err != nil {
		return 0, err
	}
	if batchSize <= 0 {
		batchSize = 500
	}
	r.mu.Lock()
	var count int64
	for id, doc := range r.documents {
		if doc.expires.Before(before) && len(r.state.DeleteIDs) < batchSize {
			r.state.DeleteIDs = append(r.state.DeleteIDs, id)
			count += int64(doc.summary.RequestCount)
		}
	}
	if len(r.state.DeleteIDs) == 0 {
		r.mu.Unlock()
		return 0, nil
	}
	expired := append([]int64(nil), r.state.DeleteIDs...)
	if err := r.saveStateLocked(); err != nil {
		r.mu.Unlock()
		return 0, err
	}
	for _, id := range expired {
		r.forgetDocumentLocked(id)
	}
	r.mu.Unlock()
	if err := r.finishDeletes(ctx); err != nil {
		return 0, err
	}
	r.mu.Lock()
	r.lastCleanup = time.Now().UTC()
	r.mu.Unlock()
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

// Archive settings (retention and cleanup cadence) live next to the archive so
// they survive restarts independently of the SQL deployment. Group capture
// selection is deliberately not stored here; it is a settings-table allowlist.
type userRequestAuditFileConfig struct {
	RetentionDays        int `json:"retention_days"`
	CleanupIntervalHours int `json:"cleanup_interval_hours"`
}

func (r *FileUserRequestAuditRepository) configPath() string {
	return filepath.Join(r.root, "config.json")
}

func (r *FileUserRequestAuditRepository) readConfig() (service.UserRequestAuditConfig, bool) {
	body, err := os.ReadFile(r.configPath())
	if err != nil {
		return service.UserRequestAuditConfig{}, false
	}
	var stored userRequestAuditFileConfig
	if err = json.Unmarshal(body, &stored); err != nil {
		return service.UserRequestAuditConfig{}, false
	}
	return service.UserRequestAuditConfig{RetentionDays: stored.RetentionDays, CleanupIntervalHours: stored.CleanupIntervalHours}, true
}

func (r *FileUserRequestAuditRepository) GetAuditConfig() service.UserRequestAuditConfig {
	cfg, ok := r.readConfig()
	if !ok {
		return service.UserRequestAuditConfig{}
	}
	return cfg
}

func (r *FileUserRequestAuditRepository) SetAuditConfig(cfg service.UserRequestAuditConfig) error {
	return auditAtomicJSON(r.configPath(), userRequestAuditFileConfig{RetentionDays: cfg.RetentionDays, CleanupIntervalHours: cfg.CleanupIntervalHours})
}

var _ service.UserRequestAuditRepository = (*FileUserRequestAuditRepository)(nil)

// StorageStatus reads disk usage and file sizes without any repository lock, so
// a status poll never waits for capture work.
func (r *FileUserRequestAuditRepository) StorageStatus() service.UserRequestAuditStorageStatus {
	out := service.UserRequestAuditStorageStatus{}
	out.DiskTotalBytes, out.DiskFreeBytes, out.PauseReason = r.diskStatus(context.Background())
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
	r.mu.Lock()
	out.LastCleanup = r.lastCleanup
	r.mu.Unlock()
	return out
}

// diskStatus returns total bytes, free bytes and a pause reason.
func (r *FileUserRequestAuditRepository) diskStatus(ctx context.Context) (int64, int64, string) {
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

// checkDisk gates every publication. It takes no repository lock.
func (r *FileUserRequestAuditRepository) checkDisk(ctx context.Context) error {
	_, _, reason := r.diskStatus(ctx)
	if reason != "" {
		return fmt.Errorf("audit recording paused: %s", reason)
	}
	return nil
}
