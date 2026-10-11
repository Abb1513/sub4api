package repository

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

func (r *FileUserRequestAuditRepository) pendingPath(id int64) string {
	return filepath.Join(r.root, "pending", strconv.FormatInt(id, 10)+".pending")
}

// Prepare every replacement before publishing a journal. Recovery completes
// all renames before indexing, so splitting/merging cannot lose rows halfway.
func (r *FileUserRequestAuditRepository) commitDocumentsLocked(ctx context.Context, docs []*auditDiskConversation, obsolete []int64) error {
	ids := make([]int64, 0, len(docs))
	for _, doc := range docs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.checkDiskLocked(ctx); err != nil {
			return err
		}
		if err := auditAtomicJSON(r.pendingPath(doc.ID), doc); err != nil {
			return err
		}
		ids = append(ids, doc.ID)
	}
	r.state.PendingIDs = ids
	r.state.ObsoleteIDs = obsolete
	if err := r.saveStateLocked(); err != nil {
		return err
	}
	return r.finishWritesLocked(ctx)
}
func (r *FileUserRequestAuditRepository) finishWritesLocked(ctx context.Context) error {
	if len(r.state.PendingIDs) == 0 && len(r.state.ObsoleteIDs) == 0 {
		return nil
	}
	for _, id := range r.state.PendingIDs {
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
	if len(r.state.PendingIDs) > 0 {
		if err := auditSyncDirectory(filepath.Join(r.root, "pending")); err != nil {
			return err
		}
	}
	for _, id := range r.state.ObsoleteIDs {
		if err := os.Remove(r.conversationPath(id)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := auditSyncDirectory(filepath.Join(r.root, "conversations")); err != nil {
		return err
	}
	r.state.PendingIDs = nil
	r.state.ObsoleteIDs = nil
	return r.saveStateLocked()
}
