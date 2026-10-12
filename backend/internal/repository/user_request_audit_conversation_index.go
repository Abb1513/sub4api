package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/MACOS-DO/sub4api/internal/service"
)

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

// candidateSummaries lists conversation summaries. With SQL present it is a
// plain indexed query and takes no repository lock at all; without SQL (tests
// and single-file deployments) it folds the association maps under mu only.
func (r *FileUserRequestAuditRepository) candidateSummaries(ctx context.Context, f service.UserRequestAuditFilter, paged bool) ([]*service.UserRequestAudit, int64, error) {
	if r.legacy == nil {
		return r.candidateSummariesFromMaps(f, paged)
	}
	return r.candidateSummariesFromSQL(ctx, f, paged)
}

func (r *FileUserRequestAuditRepository) candidateSummariesFromMaps(f service.UserRequestAuditFilter, paged bool) ([]*service.UserRequestAudit, int64, error) {
	out := make([]*service.UserRequestAudit, 0)
	noContent := f
	noContent.Q = ""
	r.mu.Lock()
	if r.index == nil {
		r.mu.Unlock()
		return nil, 0, errors.New("audit archive index is unavailable")
	}
	matched := make(map[int64]bool)
	for _, entry := range r.index {
		if auditMatchesFilter(&entry.row, noContent) {
			matched[entry.conversationID] = true
		}
	}
	for id := range matched {
		row := r.documents[id].summary
		out = append(out, &row)
	}
	r.mu.Unlock()
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
		out = out[start:min(start+size, len(out))]
	}
	return out, total, nil
}

func (r *FileUserRequestAuditRepository) candidateSummariesFromSQL(ctx context.Context, f service.UserRequestAuditFilter, paged bool) ([]*service.UserRequestAudit, int64, error) {
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

// ListConversations returns associated-conversation summaries, one row per
// conversation. With SQL present this is a plain indexed query that never waits
// for the association maps, so an archive rewrite cannot stall the list.
// Content search is the only path that touches payloads, and it reads them
// with no lock held.
func (r *FileUserRequestAuditRepository) ListConversations(ctx context.Context, f service.UserRequestAuditFilter) ([]*service.UserRequestAudit, int64, error) {
	if f.Legacy && r.legacy != nil {
		return r.legacy.List(ctx, f)
	}
	if r.legacy == nil && !r.indexReady() {
		if err := r.ensureIndex(ctx); err != nil {
			return nil, 0, err
		}
	}
	contentSearch := strings.TrimSpace(f.Q) != ""
	rows, total, err := r.candidateSummaries(ctx, f, !contentSearch)
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

// ResolveConversationIDs maps request or conversation ids to conversation ids,
// waiting only for the association index.
func (r *FileUserRequestAuditRepository) ResolveConversationIDs(ctx context.Context, ids []int64) ([]int64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if err := r.ensureIndex(ctx); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.index == nil {
		return nil, errors.New("audit archive index is unavailable")
	}
	out := make([]int64, 0, len(ids))
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		entry, ok := r.index[id]
		if !ok {
			return nil, fmt.Errorf("selected audit conversation %d no longer exists", id)
		}
		if !seen[entry.conversationID] {
			seen[entry.conversationID] = true
			out = append(out, entry.conversationID)
		}
	}
	return out, nil
}

// ConversationSummaries returns stored summaries for explicit conversation ids
// in deterministic order. No payload is decoded.
func (r *FileUserRequestAuditRepository) ConversationSummaries(ctx context.Context, ids []int64) ([]*service.UserRequestAudit, error) {
	if err := r.ensureIndex(ctx); err != nil {
		return nil, err
	}
	out := make([]*service.UserRequestAudit, 0, len(ids))
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.index == nil {
		return nil, errors.New("audit archive index is unavailable")
	}
	for _, id := range ids {
		meta, ok := r.documents[id]
		if !ok {
			return nil, fmt.Errorf("audit conversation %d no longer exists", id)
		}
		summary := meta.summary
		out = append(out, &summary)
	}
	return out, nil
}

// ForEachConversation decodes one conversation payload at a time and hands it to
// visit. Only the path lookup is lock-protected, so an export never keeps the
// archive - or captures - waiting on slow disk or a slow consumer.
func (r *FileUserRequestAuditRepository) ForEachConversation(ctx context.Context, ids []int64, visit func(*auditDiskConversation) error) error {
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		doc, err := r.loadAuditDocument(ctx, id)
		if err != nil {
			return err
		}
		if err = visit(doc); err != nil {
			return err
		}
	}
	return nil
}

// ExportConversations writes whole conversations, ignoring per-request filters
// except for the optional content match, which keeps the entire conversation.
func (r *FileUserRequestAuditRepository) ExportConversations(ctx context.Context, f service.UserRequestAuditFilter, ids []int64, w io.Writer, format string) (int, error) {
	if format != "json" && format != "jsonl" {
		return 0, fmt.Errorf("unsupported audit export format %q", format)
	}
	if f.Legacy && r.legacy != nil {
		return r.exportLegacyAuditConversations(ctx, f, ids, w, format)
	}
	candidates := ids
	if len(candidates) == 0 {
		summaries, _, err := r.candidateSummaries(ctx, f, false)
		if err != nil {
			return 0, err
		}
		candidates = make([]int64, 0, len(summaries))
		for _, summary := range summaries {
			candidates = append(candidates, summary.ID)
		}
	} else {
		resolved, err := r.ResolveConversationIDs(ctx, ids)
		if err != nil {
			return 0, err
		}
		candidates = resolved
		sort.Slice(candidates, func(i, j int) bool { return candidates[i] > candidates[j] })
	}
	summaryList, err := r.ConversationSummaries(ctx, candidates)
	if err != nil {
		return 0, err
	}
	summaries := make(map[int64]*service.UserRequestAudit, len(summaryList))
	for _, summary := range summaryList {
		summaries[summary.ID] = summary
	}
	content := strings.TrimSpace(f.Q) != ""
	if format == "json" {
		if _, err = io.WriteString(w, "[\n"); err != nil {
			return 0, err
		}
	}
	count := 0
	enc := json.NewEncoder(auditExportContextWriter{ctx: ctx, writer: w})
	err = r.ForEachConversation(ctx, candidates, func(doc *auditDiskConversation) error {
		if content {
			found := false
			for _, row := range doc.Requests {
				found = found || auditMatchesFilter(row, f)
			}
			if !found {
				return nil
			}
		}
		summary, ok := summaries[doc.ID]
		if !ok {
			return fmt.Errorf("audit conversation %d lost its summary", doc.ID)
		}
		exported := *summary
		exported.ChatML = auditMessagesTranscript(doc.Messages)
		if err := r.labelUsers(ctx, []*service.UserRequestAudit{&exported}); err != nil {
			return err
		}
		if format == "json" && count > 0 {
			if _, err := io.WriteString(w, ",\n"); err != nil {
				return err
			}
		}
		if err := enc.Encode(struct {
			*service.UserRequestAudit
			Requests []*service.UserRequestAudit       `json:"requests"`
			Messages []service.UserRequestAuditMessage `json:"messages"`
		}{&exported, doc.Requests, doc.Messages}); err != nil {
			return err
		}
		count++
		return nil
	})
	if err != nil {
		return count, err
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

