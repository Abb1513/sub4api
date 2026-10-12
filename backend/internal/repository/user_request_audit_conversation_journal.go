package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/MACOS-DO/sub4api/internal/service"
)

// Locking contract (see the note in user_request_audit_file.go):
//
//	mu    guards the association maps and the archive journal; sections contain
//	      map/state operations only, never payload encoding, renames or SQL.
//	ioMu  serializes archive mutations. Readers never take it.
//
// Every helper here snapshots what it needs under mu, performs I/O with no lock
// held, then publishes the result under mu again.

func (r *FileUserRequestAuditRepository) pendingPath(id int64) string {
	return filepath.Join(r.root, "pending", strconv.FormatInt(id, 10)+".pending")
}

// commitDocuments publishes document replacements durably. Callers hold ioMu.
// Encoding and renaming happen without mu, so list/read requests are never
// queued behind a large conversation rewrite.
func (r *FileUserRequestAuditRepository) commitDocuments(ctx context.Context, docs []*auditDiskConversation, obsolete []int64) error {
	ids := make([]int64, 0, len(docs))
	for _, doc := range docs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.checkDisk(ctx); err != nil {
			return err
		}
		if err := auditAtomicJSON(r.pendingPath(doc.ID), doc); err != nil {
			r.invalidateDocuments(doc.ID)
			return err
		}
		ids = append(ids, doc.ID)
	}

	// Journal the intent before publishing, so recovery can finish by retry.
	r.mu.Lock()
	r.state.PendingIDs = ids
	r.state.ObsoleteIDs = obsolete
	if err := r.saveStateLocked(); err != nil {
		r.mu.Unlock()
		r.invalidateDocuments(append(append([]int64(nil), ids...), obsolete...)...)
		return err
	}
	r.mu.Unlock()

	if err := r.finishWrites(ctx); err != nil {
		r.invalidateDocuments(append(append([]int64(nil), ids...), obsolete...)...)
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range obsolete {
		r.forgetDocumentLocked(id)
	}
	for _, doc := range docs {
		r.indexDocumentLocked(doc)
	}
	return nil
}

// finishWrites applies the publication journal. Callers hold ioMu and must not
// hold mu. It is idempotent: a timed-out writer can retry without losing rows.
func (r *FileUserRequestAuditRepository) finishWrites(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	pending := append([]int64(nil), r.state.PendingIDs...)
	obsolete := append([]int64(nil), r.state.ObsoleteIDs...)
	r.mu.Unlock()
	if len(pending) == 0 && len(obsolete) == 0 {
		return nil
	}
	targets := filepath.Join(r.root, "conversations")
	if len(pending) > 0 {
		// A fresh archive has no conversations directory yet, and a rename into
		// a missing directory fails outright on Windows.
		if err := os.MkdirAll(targets, 0o750); err != nil {
			return err
		}
	}
	for _, id := range pending {
		if err := ctx.Err(); err != nil {
			return err
		}
		source, target := r.pendingPath(id), r.conversationPath(id)
		if _, err := os.Stat(source); os.IsNotExist(err) {
			if _, err = readAuditConversation(target); err != nil {
				return fmt.Errorf("recover audit publication %d: %w", id, err)
			}
			continue
		} else if err != nil {
			return err
		}
		if err := os.Rename(source, target); err != nil {
			return err
		}
	}
	if len(pending) > 0 {
		if err := auditSyncDirectory(filepath.Join(r.root, "pending")); err != nil {
			return err
		}
	}
	for _, id := range obsolete {
		if err := os.Remove(r.conversationPath(id)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := auditSyncDirectory(filepath.Join(r.root, "conversations")); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.PendingIDs = nil
	r.state.ObsoleteIDs = nil
	return r.saveStateLocked()
}

// finishDeletes applies the deletion journal. Callers hold ioMu and must not
// hold mu while files or SQL are touched.
func (r *FileUserRequestAuditRepository) finishDeletes(ctx context.Context) error {
	r.mu.Lock()
	ids := append([]int64(nil), r.state.DeleteIDs...)
	clearing := r.state.Clear
	r.mu.Unlock()
	if !clearing && len(ids) == 0 {
		return nil
	}
	if clearing {
		files, err := filepath.Glob(filepath.Join(r.root, "conversations", "*.json"))
		if err != nil {
			return err
		}
		for _, path := range files {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
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
	if clearing {
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
	if err := auditSyncDirectory(filepath.Join(r.root, "conversations")); err != nil {
		return err
	}
	if r.legacy != nil {
		if clearing {
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
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.Clear = false
	r.state.DeleteIDs = nil
	return r.saveStateLocked()
}

// syncSQLDocuments projects metadata for the supplied documents. No repository
// lock is held during SQL I/O; each document's metadata is snapshotted first.
func (r *FileUserRequestAuditRepository) syncSQLDocuments(ctx context.Context, docs []*auditDiskConversation, removed []int64, replace bool) error {
	if r.legacy == nil {
		return nil
	}
	tx, err := r.legacy.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if replace {
		if _, err = tx.ExecContext(ctx, "DELETE FROM user_request_audit_conversations"); err != nil {
			return err
		}
	}
	for _, id := range removed {
		if _, err = tx.ExecContext(ctx, "DELETE FROM user_request_audit_conversations WHERE id=$1", id); err != nil {
			return err
		}
	}
	for _, doc := range docs {
		if err := ctx.Err(); err != nil {
			return err
		}
		r.mu.Lock()
		meta := r.documents[doc.ID]
		r.mu.Unlock()
		summary, err := json.Marshal(meta.summary)
		if err != nil {
			return err
		}
		relative := "conversations/" + strconv.FormatInt(doc.ID, 10) + ".json"
		_, err = tx.ExecContext(ctx, `INSERT INTO user_request_audit_conversations(id,user_id,api_key_id,created_at,updated_at,expires_at,request_count,relative_path,summary) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb) ON CONFLICT(id) DO UPDATE SET user_id=EXCLUDED.user_id,api_key_id=EXCLUDED.api_key_id,created_at=EXCLUDED.created_at,updated_at=EXCLUDED.updated_at,expires_at=EXCLUDED.expires_at,request_count=EXCLUDED.request_count,relative_path=EXCLUDED.relative_path,summary=EXCLUDED.summary`, doc.ID, meta.summary.UserID, meta.summary.APIKeyID, meta.summary.CreatedAt, meta.summary.UpdatedAt, meta.expires, meta.summary.RequestCount, relative, string(summary))
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM user_request_audit_request_index WHERE conversation_id=$1", doc.ID); err != nil {
			return err
		}
		for _, row := range doc.Requests {
			metadata := auditMetadata(row)
			body, err := json.Marshal(metadata)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO user_request_audit_request_index(id,conversation_id,logical_key,created_at,metadata) VALUES($1,$2,$3,$4,$5::jsonb) ON CONFLICT(id) DO UPDATE SET conversation_id=EXCLUDED.conversation_id,logical_key=EXCLUDED.logical_key,created_at=EXCLUDED.created_at,metadata=EXCLUDED.metadata`, row.ID, doc.ID, row.LogicalKey, row.CreatedAt, string(body)); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// reconcileSQL rebuilds the whole metadata projection from the association maps.
func (r *FileUserRequestAuditRepository) reconcileSQL(ctx context.Context) error {
	if r.legacy == nil {
		return nil
	}
	type projectedRow struct {
		conversationID int64
		metadata       service.UserRequestAudit
	}
	r.mu.Lock()
	rows := make([]projectedRow, 0, len(r.index))
	for _, entry := range r.index {
		rows = append(rows, projectedRow{conversationID: entry.conversationID, metadata: entry.row})
	}
	ids := make([]int64, 0, len(r.documents))
	for id := range r.documents {
		ids = append(ids, id)
	}
	r.mu.Unlock()

	docs := make([]*auditDiskConversation, 0, len(ids))
	positions := make(map[int64]int, len(ids))
	for _, id := range ids {
		positions[id] = len(docs)
		docs = append(docs, &auditDiskConversation{UserRequestAuditConversation: &service.UserRequestAuditConversation{ID: id}})
	}
	for i := range rows {
		metadata := rows[i].metadata
		doc := docs[positions[rows[i].conversationID]]
		doc.Requests = append(doc.Requests, &metadata)
	}
	return r.syncSQLDocuments(ctx, docs, nil, true)
}
