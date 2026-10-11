package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/MACOS-DO/sub4api/internal/service"
)

func (r *FileUserRequestAuditRepository) syncSQLDocumentsLocked(ctx context.Context, docs []*auditDiskConversation, removed []int64, replace bool) error {
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
		meta := r.documents[doc.ID]
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
			_, err = tx.ExecContext(ctx, `INSERT INTO user_request_audit_request_index(id,conversation_id,logical_key,created_at,metadata) VALUES($1,$2,$3,$4,$5::jsonb) ON CONFLICT(id) DO UPDATE SET conversation_id=EXCLUDED.conversation_id,logical_key=EXCLUDED.logical_key,created_at=EXCLUDED.created_at,metadata=EXCLUDED.metadata`, row.ID, doc.ID, row.LogicalKey, row.CreatedAt, string(body))
			if err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
func (r *FileUserRequestAuditRepository) reconcileSQLLocked(ctx context.Context) error {
	if r.legacy == nil {
		return nil
	}
	docs := make([]*auditDiskConversation, 0, len(r.documents))
	for id := range r.documents {
		docs = append(docs, &auditDiskConversation{UserRequestAuditConversation: &service.UserRequestAuditConversation{ID: id}})
	}
	positions := make(map[int64]int, len(docs))
	for i, doc := range docs {
		positions[doc.ID] = i
	}
	for _, entry := range r.index {
		row := entry.row
		doc := docs[positions[entry.conversationID]]
		doc.Requests = append(doc.Requests, &row)
	}
	return r.syncSQLDocumentsLocked(ctx, docs, nil, true)
}
func auditSQLFilter(f service.UserRequestAuditFilter) (string, []any) {
	return auditSQLConditions(f, false)
}
func auditSQLConditions(f service.UserRequestAuditFilter, requestOnly bool) (string, []any) {
	args := make([]any, 0)
	where := make([]string, 0)
	add := func(expr string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(expr, len(args)))
	}
	if f.UserID != nil {
		add("c.user_id=$%d", *f.UserID)
	}
	if f.APIKeyID != nil {
		add("c.api_key_id=$%d", *f.APIKeyID)
	}
	request := make([]string, 0)
	addRequest := func(expr string, value any) {
		args = append(args, value)
		request = append(request, fmt.Sprintf(expr, len(args)))
	}
	if f.GroupID != nil {
		addRequest("(q.metadata->>'group_id')::bigint=$%d", *f.GroupID)
	}
	for _, pair := range [][2]string{{"group_name", f.GroupName}, {"protocol", f.Protocol}, {"requested_model", f.RequestedModel}, {"response_id", f.ResponseID}, {"client_request_id", f.ClientRequestID}, {"status", f.Status}} {
		if pair[1] != "" {
			addRequest("lower(q.metadata->>'"+pair[0]+"')=lower($%d)", pair[1])
		}
	}
	if f.StartTime != nil {
		addRequest("q.created_at >= $%d", *f.StartTime)
	}
	if f.EndTime != nil {
		addRequest("q.created_at < $%d", *f.EndTime)
	}
	if requestOnly {
		where = append(where, request...)
	} else if len(request) > 0 {
		where = append(where, "EXISTS (SELECT 1 FROM user_request_audit_request_index q WHERE q.conversation_id=c.id AND "+strings.Join(request, " AND ")+")")
	}
	if len(where) == 0 {
		return "TRUE", args
	}
	return strings.Join(where, " AND "), args
}
func auditPage(f service.UserRequestAuditFilter) (int, int) {
	page, size := f.Page, f.PageSize
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 20
	}
	if size > 200 {
		size = 200
	}
	return page, size
}
func (r *FileUserRequestAuditRepository) candidateSummariesLocked(ctx context.Context, f service.UserRequestAuditFilter, paged bool) ([]*service.UserRequestAudit, int64, error) {
	if r.legacy == nil {
		out := make([]*service.UserRequestAudit, 0)
		matched := make(map[int64]bool)
		noContent := f
		noContent.Q = ""
		for _, entry := range r.index {
			if auditMatchesFilter(&entry.row, noContent) {
				matched[entry.conversationID] = true
			}
		}
		for id := range matched {
			row := r.documents[id].summary
			out = append(out, &row)
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
				return out[i].ID > out[j].ID
			}
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		})
		total := int64(len(out))
		if paged {
			page, size := auditPage(f)
			if page-1 > len(out)/size {
				return []*service.UserRequestAudit{}, total, nil
			}
			start := (page - 1) * size
			end := min(start+size, len(out))
			out = out[start:end]
		}
		return out, total, nil
	}
	where, args := auditSQLFilter(f)
	var total int64
	if err := r.legacy.db.QueryRowContext(ctx, "SELECT count(*) FROM user_request_audit_conversations c WHERE "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	query := "SELECT c.summary FROM user_request_audit_conversations c WHERE " + where + " ORDER BY c.updated_at DESC,c.id DESC"
	if paged {
		page, size := auditPage(f)
		args = append(args, size, int64(page-1)*int64(size))
		query += fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)-1, len(args))
	}
	rows, err := r.legacy.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]*service.UserRequestAudit, 0)
	for rows.Next() {
		var b []byte
		if err = rows.Scan(&b); err != nil {
			return nil, 0, err
		}
		var row service.UserRequestAudit
		if err = json.Unmarshal(b, &row); err != nil {
			return nil, 0, err
		}
		out = append(out, &row)
	}
	return out, total, rows.Err()
}
func (r *FileUserRequestAuditRepository) ListConversations(ctx context.Context, f service.UserRequestAuditFilter) ([]*service.UserRequestAudit, int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureIndexLocked(ctx); err != nil {
		return nil, 0, err
	}
	if f.Legacy && r.legacy != nil {
		return r.legacy.List(ctx, f)
	}
	contentSearch := strings.TrimSpace(f.Q) != ""
	rows, total, err := r.candidateSummariesLocked(ctx, f, !contentSearch)
	if err != nil {
		return nil, 0, err
	}
	if contentSearch {
		matched := rows[:0]
		for _, summary := range rows {
			if err = ctx.Err(); err != nil {
				return nil, 0, err
			}
			doc, e := readAuditConversation(r.conversationPath(summary.ID))
			if e != nil {
				return nil, 0, e
			}
			for _, request := range doc.Requests {
				if auditMatchesFilter(request, f) {
					matched = append(matched, summary)
					break
				}
			}
		}
		total = int64(len(matched))
		page, size := auditPage(f)
		if page-1 > len(matched)/size {
			rows = []*service.UserRequestAudit{}
		} else {
			start := (page - 1) * size
			rows = matched[start:min(start+size, len(matched))]
		}
	}
	if err = r.labelUsers(ctx, rows); err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

type auditConversationExportFile struct {
	file    *os.File
	summary *service.UserRequestAudit
}

func (r *FileUserRequestAuditRepository) ExportConversations(ctx context.Context, f service.UserRequestAuditFilter, ids []int64, w io.Writer, format string) (int, error) {
	if format != "json" && format != "jsonl" {
		return 0, fmt.Errorf("unsupported audit export format %q", format)
	}
	if f.Legacy && r.legacy != nil {
		return r.exportLegacyAuditConversations(ctx, f, ids, w, format)
	}
	// Pin each atomically replaced file under the capture mutex. Slow consumers
	// decode pinned versions after releasing the mutex; capture remains available.
	var snapshot []auditConversationExportFile
	defer func() {
		for _, entry := range snapshot {
			if entry.file != nil {
				_ = entry.file.Close()
			}
		}
	}()
	err := func() error {
		r.mu.Lock()
		defer r.mu.Unlock()
		if err := r.ensureIndexLocked(ctx); err != nil {
			return err
		}
		selected := make(map[int64]bool, len(ids))
		for _, id := range ids {
			entry, ok := r.index[id]
			if !ok {
				return fmt.Errorf("selected audit conversation %d no longer exists", id)
			}
			selected[entry.conversationID] = true
		}
		summaries, _, err := r.candidateSummariesLocked(ctx, f, false)
		if err != nil {
			return err
		}
		for _, summary := range summaries {
			if len(ids) > 0 && !selected[summary.ID] {
				continue
			}
			if err = ctx.Err(); err != nil {
				return err
			}
			file, e := openAuditSnapshot(r.conversationPath(summary.ID))
			if e != nil {
				return e
			}
			snapshot = append(snapshot, auditConversationExportFile{file: file, summary: summary})
		}
		return nil
	}()
	if err != nil {
		return 0, err
	}
	if format == "json" {
		if _, err = io.WriteString(w, "[\n"); err != nil {
			return 0, err
		}
	}
	count := 0
	enc := json.NewEncoder(auditExportContextWriter{ctx: ctx, writer: w})
	for i := range snapshot {
		entry := &snapshot[i]
		if err = ctx.Err(); err != nil {
			return count, err
		}
		var doc auditDiskConversation
		if err = json.NewDecoder(entry.file).Decode(&doc); err != nil {
			return count, err
		}
		if err = entry.file.Close(); err != nil {
			return count, err
		}
		entry.file = nil
		if strings.TrimSpace(f.Q) != "" {
			found := false
			for _, row := range doc.Requests {
				found = found || auditMatchesFilter(row, f)
			}
			if !found {
				continue
			}
		}
		entry.summary.ChatML = auditMessagesTranscript(doc.Messages)
		if format == "json" && count > 0 {
			if _, err = io.WriteString(w, ",\n"); err != nil {
				return count, err
			}
		}
		if err = enc.Encode(struct {
			*service.UserRequestAudit
			Requests []*service.UserRequestAudit       `json:"requests"`
			Messages []service.UserRequestAuditMessage `json:"messages"`
		}{entry.summary, doc.Requests, doc.Messages}); err != nil {
			return count, err
		}
		count++
	}
	if format == "json" {
		_, err = io.WriteString(w, "]\n")
	}
	return count, err
}

type auditExportContextWriter struct {
	ctx    context.Context
	writer io.Writer
}

func (w auditExportContextWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.writer.Write(p)
}
