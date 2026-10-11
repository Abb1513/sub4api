package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/MACOS-DO/sub4api/internal/service"
)

// List retains the repository's request-level contract for statistics. The
// service's public conversation listing calls ListConversations separately.
func (r *FileUserRequestAuditRepository) List(ctx context.Context, f service.UserRequestAuditFilter) ([]*service.UserRequestAudit, int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureIndexLocked(ctx); err != nil {
		return nil, 0, err
	}
	if f.Legacy && r.legacy != nil {
		return r.legacy.List(ctx, f)
	}
	content := strings.TrimSpace(f.Q) != ""
	page, size := auditPage(f)
	rows := make([]*service.UserRequestAudit, 0)
	var total int64
	if r.legacy == nil {
		noContent := f
		noContent.Q = ""
		for _, entry := range r.index {
			if auditMatchesFilter(&entry.row, noContent) {
				row := entry.row
				rows = append(rows, &row)
			}
		}
		sort.Slice(rows, func(i, j int) bool { return auditRowBefore(rows[j], rows[i]) })
		total = int64(len(rows))
	} else {
		where, args := auditSQLConditions(f, true)
		from := " FROM user_request_audit_request_index q JOIN user_request_audit_conversations c ON c.id=q.conversation_id WHERE " + where
		if err := r.legacy.db.QueryRowContext(ctx, "SELECT count(*)"+from, args...).Scan(&total); err != nil {
			return nil, 0, err
		}
		query := "SELECT q.metadata" + from + " ORDER BY q.created_at DESC,q.id DESC"
		if !content {
			args = append(args, size, int64(page-1)*int64(size))
			query += fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)-1, len(args))
		}
		result, err := r.legacy.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, 0, err
		}
		for result.Next() {
			var b []byte
			if err = result.Scan(&b); err != nil {
				result.Close()
				return nil, 0, err
			}
			var row service.UserRequestAudit
			if err = json.Unmarshal(b, &row); err != nil {
				result.Close()
				return nil, 0, err
			}
			rows = append(rows, &row)
		}
		err = result.Err()
		result.Close()
		if err != nil {
			return nil, 0, err
		}
	}
	if content {
		filtered := rows[:0]
		loaded := make(map[int64]*auditDiskConversation)
		for _, row := range rows {
			if err := ctx.Err(); err != nil {
				return nil, 0, err
			}
			entry := r.index[row.ID]
			doc := loaded[entry.conversationID]
			if doc == nil {
				var err error
				doc, err = readAuditConversation(r.conversationPath(entry.conversationID))
				if err != nil {
					return nil, 0, err
				}
				loaded[entry.conversationID] = doc
			}
			for _, full := range doc.Requests {
				if full.ID == row.ID && auditMatchesFilter(full, f) {
					filtered = append(filtered, row)
					break
				}
			}
		}
		rows = filtered
		total = int64(len(rows))
	}
	if content || r.legacy == nil {
		if page-1 > len(rows)/size {
			return []*service.UserRequestAudit{}, total, nil
		}
		start := (page - 1) * size
		rows = rows[start:min(start+size, len(rows))]
	}
	return rows, total, nil
}
