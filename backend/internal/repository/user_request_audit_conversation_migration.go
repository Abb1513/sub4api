package repository

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/MACOS-DO/sub4api/internal/service"
)

// The old append-only format is read only by this one-time importer. There is
// deliberately no legacy writer and no legacy archive fallback after cutover.
type userRequestAuditFileLine struct {
	Event      string `json:"event"`
	LogicalKey string `json:"logical_key"`
	*service.UserRequestAudit
}

func (r *FileUserRequestAuditRepository) importLegacyLocked(ctx context.Context, latest map[int64]*service.UserRequestAudit) error {
	files, err := filepath.Glob(filepath.Join(r.root, "*.ndjson"))
	if err != nil {
		return err
	}
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		reader := bufio.NewReader(f)
		lineNumber := 0
		for {
			if err = ctx.Err(); err != nil {
				f.Close()
				return err
			}
			line, readErr := reader.ReadBytes('\n')
			lineNumber++
			if len(bytes.TrimSpace(line)) > 0 {
				var record userRequestAuditFileLine
				if err = json.Unmarshal(line, &record); err != nil {
					f.Close()
					return fmt.Errorf("legacy audit %s line %d: %w", filepath.Base(path), lineNumber, err)
				}
				row := record.UserRequestAudit
				if row == nil || row.ID <= 0 {
					f.Close()
					return fmt.Errorf("invalid legacy audit row %s:%d", filepath.Base(path), lineNumber)
				}
				row.LogicalKey = record.LogicalKey
				if old := latest[row.ID]; old == nil || !old.UpdatedAt.After(row.UpdatedAt) {
					latest[row.ID] = row
				}
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				f.Close()
				return readErr
			}
		}
		if err = f.Close(); err != nil {
			return err
		}
	}
	// Older archives could replace a logical request with a different numeric ID.
	// Preserve the latest snapshot within owner/API-key scope, never across it.
	logical := make(map[string]*service.UserRequestAudit)
	for id, row := range latest {
		if row.LogicalKey == "" {
			continue
		}
		key := strconv.FormatInt(row.UserID, 10) + "/" + strconv.FormatInt(row.APIKeyID, 10) + "/" + row.LogicalKey
		if old := logical[key]; old != nil {
			if old.UpdatedAt.After(row.UpdatedAt) || (old.UpdatedAt.Equal(row.UpdatedAt) && old.ID > row.ID) {
				delete(latest, id)
				continue
			}
			delete(latest, old.ID)
		}
		logical[key] = row
	}
	rows := make([]*service.UserRequestAudit, 0, len(latest))
	for _, row := range latest {
		rows = append(rows, row)
	}
	if len(files) > 0 {
		if err = r.replaceAllDocumentsLocked(ctx, rows); err != nil {
			return err
		}
	}
	// Index publication and every verified document precede moving any original.
	if len(files) > 0 {
		if err = r.reconcileSQLLocked(ctx); err != nil {
			return err
		}
	}
	if len(files) > 0 {
		backup := r.root + "-migration-backup"
		if err = os.MkdirAll(backup, 0o750); err != nil {
			return err
		}
		for _, path := range files {
			destination := filepath.Join(backup, filepath.Base(path))
			if _, e := os.Stat(destination); e == nil {
				return fmt.Errorf("legacy backup already exists: %s", filepath.Base(path))
			} else if !os.IsNotExist(e) {
				return e
			}
			if err = os.Rename(path, destination); err != nil {
				return err
			}
		}
		if err = auditSyncDirectory(backup); err != nil {
			return err
		}
		if err = auditSyncDirectory(r.root); err != nil {
			return err
		}
		if err = auditSyncDirectory(filepath.Dir(r.root)); err != nil {
			return err
		}
	}
	r.state.Migrated = true
	return r.saveStateLocked()
}
func (r *FileUserRequestAuditRepository) replaceAllDocumentsLocked(ctx context.Context, rows []*service.UserRequestAudit) error {
	groups := groupAuditConversations(rows)
	docs := make([]*auditDiskConversation, 0, len(groups))
	keep := make(map[int64]bool, len(groups))
	for _, group := range groups {
		doc := r.makeDocument(group.rows, group.representative.ID)
		keep[doc.ID] = true
		docs = append(docs, doc)
	}
	files, err := filepath.Glob(filepath.Join(r.root, "conversations", "*.json"))
	if err != nil {
		return err
	}
	obsolete := make([]int64, 0)
	for _, path := range files {
		id, err := strconv.ParseInt(filepath.Base(path)[:len(filepath.Base(path))-len(".json")], 10, 64)
		if err != nil {
			return err
		}
		if !keep[id] {
			obsolete = append(obsolete, id)
		}
	}
	if err = r.commitDocumentsLocked(ctx, docs, obsolete); err != nil {
		return err
	}
	for _, doc := range docs {
		verified, err := readAuditConversation(r.conversationPath(doc.ID))
		if err != nil {
			return err
		}
		want, err := json.Marshal(doc)
		if err != nil {
			return err
		}
		got, err := json.Marshal(verified)
		if err != nil {
			return err
		}
		if !bytes.Equal(want, got) {
			return fmt.Errorf("audit conversation %d verification mismatch", doc.ID)
		}
	}
	r.index = make(map[int64]auditArchiveEntry)
	r.documents = make(map[int64]auditDocumentMeta)
	r.logicalIndex = make(map[string]int64)
	for _, doc := range docs {
		r.indexDocumentLocked(doc)
	}
	return nil
}
