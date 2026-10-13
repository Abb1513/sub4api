package repository

import (
	"context"
	"fmt"

	"github.com/MACOS-DO/sub4api/internal/service"
)

// List supplies request-level metadata for statistics. Text search never opens
// cold evidence; explicit GetByID/GetConversation/export are the only readers.
func (r *FileUserRequestAuditRepository) List(ctx context.Context, f service.UserRequestAuditFilter) ([]*service.UserRequestAudit, int64, error) {
	if err := r.requireDB(); err != nil {
		return nil, 0, err
	}
	where, args := auditSQLConditions(f, true)
	from := " FROM user_request_audit_request_index q JOIN user_request_audit_conversations c ON c.id=q.conversation_id WHERE " + where
	var total int64
	if err := r.db.QueryRowContext(ctx, "SELECT count(*)"+from, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	page, size := auditPage(f)
	args = append(args, size, int64(page-1)*int64(size))
	result, err := r.db.QueryContext(ctx, "SELECT q.metadata"+from+fmt.Sprintf(" ORDER BY q.created_at DESC,q.id DESC LIMIT $%d OFFSET $%d", len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	rows, err := readAuditMetadataRows(result)
	return rows, total, err
}
