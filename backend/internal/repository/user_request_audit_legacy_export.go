package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/MACOS-DO/sub4api/internal/service"
)

// Explicit legacy export remains read-only; no new payload is ever written to
// the old SQL table. Active capture and ordinary exports use conversation JSON.
func (r *FileUserRequestAuditRepository) exportLegacyAuditConversations(ctx context.Context, f service.UserRequestAuditFilter, ids []int64, w io.Writer, format string) (int, error) {
	result, err := r.legacy.db.QueryContext(ctx, "SELECT "+auditSelectColumns+" FROM user_request_audits ORDER BY created_at,id")
	if err != nil {
		return 0, err
	}
	var rows []*service.UserRequestAudit
	seen := make(map[int64]bool)
	for result.Next() {
		row, e := scanUserRequestAudit(result)
		if e != nil {
			result.Close()
			return 0, e
		}
		rows = append(rows, row)
		seen[row.ID] = true
	}
	err = result.Err()
	result.Close()
	if err != nil {
		return 0, err
	}
	selected := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if !seen[id] {
			return 0, fmt.Errorf("selected legacy audit conversation %d no longer exists", id)
		}
		selected[id] = true
	}
	if format == "json" {
		if _, err = io.WriteString(w, "[\n"); err != nil {
			return 0, err
		}
	}
	encoder := json.NewEncoder(auditExportContextWriter{ctx: ctx, writer: w})
	count := 0
	for _, group := range groupAuditConversations(rows) {
		matches, chosen := false, len(ids) == 0
		for _, row := range group.rows {
			matches = matches || auditMatchesFilter(row, f)
			chosen = chosen || selected[row.ID]
		}
		if !matches || !chosen {
			continue
		}
		summary := auditConversationSummary(group)
		messages := service.BuildUserRequestAuditTranscript(group.rows)
		summary.ChatML = auditMessagesTranscript(messages)
		if format == "json" && count > 0 {
			if _, err = io.WriteString(w, ",\n"); err != nil {
				return count, err
			}
		}
		if err = encoder.Encode(struct {
			*service.UserRequestAudit
			Requests []*service.UserRequestAudit       `json:"requests"`
			Messages []service.UserRequestAuditMessage `json:"messages"`
		}{summary, group.rows, messages}); err != nil {
			return count, err
		}
		count++
	}
	if format == "json" {
		_, err = io.WriteString(w, "]\n")
	}
	return count, err
}
