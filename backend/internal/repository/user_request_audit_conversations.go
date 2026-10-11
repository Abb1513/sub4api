package repository

import (
	"context"
	"fmt"
	"github.com/MACOS-DO/sub4api/internal/service"
	"sort"
	"strconv"
	"strings"
)

// Every edge is scoped to both the owner and API key. The same response ID or
// caller-supplied conversation key in another scope cannot join histories.
type auditConversationLink struct {
	userID   int64
	apiKeyID int64
	kind     byte
	value    string
}

type auditConversation struct {
	rows           []*service.UserRequestAudit
	representative *service.UserRequestAudit
	latest         *service.UserRequestAudit
}

func groupAuditConversations(rows []*service.UserRequestAudit) []auditConversation {
	return groupAuditConversationsWithProof(rows, nil)
}

func groupAuditConversationsWithProof(rows []*service.UserRequestAudit, proofs map[int64]service.UserRequestAuditContinuityProof) []auditConversation {
	parents := make([]int, len(rows))
	for i := range parents {
		parents[i] = i
	}
	var root func(int) int
	root = func(i int) int {
		for parents[i] != i {
			parents[i] = parents[parents[i]]
			i = parents[i]
		}
		return i
	}
	links := make(map[auditConversationLink]int, len(rows)*2)
	join := func(i int, kind byte, value string) {
		if value == "" {
			return
		}
		row := rows[i]
		key := auditConversationLink{row.UserID, row.APIKeyID, kind, value}
		if previous, ok := links[key]; ok {
			parents[root(i)] = root(previous)
		} else {
			links[key] = i
		}
	}
	for i, row := range rows {
		join(i, 'r', row.ResponseID)
		join(i, 'r', row.PreviousResponseID)
		// A fallback hash (including identical first questions) is not identity.
		join(i, 's', auditSessionID(row))
		if strings.HasPrefix(row.ConversationKey, "response:") {
			join(i, 'r', strings.TrimPrefix(row.ConversationKey, "response:"))
		}
	}
	// Stateless legacy continuation requires an exact complete multi-message
	// prefix and a unique edge in both directions. Sibling branches stay apart.
	terminals := make(map[auditConversationLink][]int)
	if proofs == nil {
		proofs = make(map[int64]service.UserRequestAuditContinuityProof, len(rows))
		for _, row := range rows {
			proofs[row.ID] = service.UserRequestAuditProof(row)
		}
	}
	for i, row := range rows {
		if auditSessionID(row) != "" {
			continue
		}
		if key := proofs[row.ID].Terminal; key != "" {
			link := auditConversationLink{row.UserID, row.APIKeyID, 'p', key}
			terminals[link] = append(terminals[link], i)
		}
	}
	parentsByChild := make(map[int]int)
	childrenByParent := make(map[int]int)
	for i, row := range rows {
		if auditSessionID(row) != "" {
			continue
		}
		candidate := -1
		for _, prefix := range proofs[row.ID].Prefixes {
			atBoundary := -1
			for _, p := range terminals[auditConversationLink{row.UserID, row.APIKeyID, 'p', prefix}] {
				if !auditRowBefore(rows[p], row) {
					continue
				}
				if atBoundary == -1 {
					atBoundary = p
				} else if root(atBoundary) != root(p) {
					atBoundary = -2
					break
				}
			}
			// Prefer the longest exact boundary rather than earlier ancestors.
			if atBoundary != -1 {
				candidate = atBoundary
			}
		}
		if candidate >= 0 {
			parentsByChild[i] = candidate
			childrenByParent[candidate]++
		}
	}
	for child, parent := range parentsByChild {
		if childrenByParent[parent] == 1 {
			parents[root(child)] = root(parent)
		}
	}
	groups := make(map[int]int)
	out := make([]auditConversation, 0)
	for i, row := range rows {
		id := root(i)
		group, ok := groups[id]
		if !ok {
			group = len(out)
			groups[id] = group
			out = append(out, auditConversation{})
		}
		out[group].rows = append(out[group].rows, row)
	}
	for i := range out {
		group := &out[i]
		sort.Slice(group.rows, func(i, j int) bool { return auditRowBefore(group.rows[i], group.rows[j]) })
		group.representative = group.rows[0]
		group.latest = group.rows[len(group.rows)-1]
	}
	sort.Slice(out, func(i, j int) bool { return auditRowBefore(out[j].latest, out[i].latest) })
	return out
}

func auditSessionID(row *service.UserRequestAudit) string {
	if row.SessionID != "" {
		return row.SessionID
	}
	for _, key := range []string{"session_id", "conversation_id"} {
		if value, ok := row.Metadata[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	if strings.HasPrefix(row.ConversationKey, "session:") {
		return strings.TrimPrefix(row.ConversationKey, "session:")
	}
	return ""
}

func auditRowBefore(a, b *service.UserRequestAudit) bool {
	if a.CreatedAt.Equal(b.CreatedAt) {
		return a.ID < b.ID
	}
	return a.CreatedAt.Before(b.CreatedAt)
}

func auditMatchesFilter(row *service.UserRequestAudit, filter service.UserRequestAuditFilter) bool {
	if filter.UserID != nil && row.UserID != *filter.UserID || filter.APIKeyID != nil && row.APIKeyID != *filter.APIKeyID || filter.GroupID != nil && !sameGroup(row.GroupID, filter.GroupID) || filter.GroupName != "" && !strings.EqualFold(row.GroupName, filter.GroupName) || filter.Protocol != "" && !strings.EqualFold(row.Protocol, filter.Protocol) || filter.RequestedModel != "" && !strings.EqualFold(row.RequestedModel, filter.RequestedModel) || filter.ResponseID != "" && !strings.EqualFold(row.ResponseID, filter.ResponseID) || filter.ClientRequestID != "" && !strings.EqualFold(row.ClientRequestID, filter.ClientRequestID) || filter.Status != "" && !strings.EqualFold(row.Status, filter.Status) {
		return false
	}
	if filter.StartTime != nil && row.CreatedAt.Before(*filter.StartTime) || filter.EndTime != nil && !row.CreatedAt.Before(*filter.EndTime) {
		return false
	}
	if q := strings.ToLower(strings.TrimSpace(filter.Q)); q != "" {
		blob := strings.ToLower(row.GroupName + " " + row.Protocol + " " + row.RequestedModel + " " + row.ClientRequestID + " " + row.ResponseID + " " + row.LastError + " " + string(mustJSON(row.Metadata)) + " " + row.RequestChatML + " " + row.ResponseChatML)
		return strings.Contains(blob, q)
	}
	return true
}

func auditConversationSummary(group auditConversation) *service.UserRequestAudit {
	row := *group.latest
	row.ID = group.representative.ID
	row.CreatedAt = group.representative.CreatedAt
	row.RequestCount = len(group.rows)
	row.RequestChatML, row.ResponseChatML, row.ChatML = "", "", ""
	row.ConversationKey = group.representative.ConversationKey
	if group.representative.ResponseID != "" && (row.ConversationKey == "" || strings.HasPrefix(row.ConversationKey, "fallback:") || strings.HasPrefix(row.ConversationKey, "request:")) {
		row.ConversationKey = "response:" + group.representative.ResponseID
	}
	return &row
}

func (r *FileUserRequestAuditRepository) labelUsers(ctx context.Context, rows []*service.UserRequestAudit) error {
	ids := make(map[int64]bool)
	args := make([]any, 0)
	placeholders := make([]string, 0)
	for _, row := range rows {
		row.UserLabel = "#" + strconv.FormatInt(row.UserID, 10)
		if !ids[row.UserID] {
			ids[row.UserID] = true
			args = append(args, row.UserID)
			placeholders = append(placeholders, "$"+strconv.Itoa(len(args)))
		}
	}
	if len(args) == 0 || r.legacy == nil {
		return nil
	}
	users, err := r.legacy.db.QueryContext(ctx, `SELECT id,COALESCE(NULLIF(username,''),email,'') FROM users WHERE id IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return err
	}
	defer users.Close()
	labels := make(map[int64]string)
	for users.Next() {
		var id int64
		var name string
		if err := users.Scan(&id, &name); err != nil {
			return err
		}
		if name != "" {
			labels[id] = name + " (#" + strconv.FormatInt(id, 10) + ")"
		}
	}
	if err := users.Err(); err != nil {
		return err
	}
	for _, row := range rows {
		if label := labels[row.UserID]; label != "" {
			row.UserLabel = label
		}
	}
	return nil
}

// Rendered export and reader use the same protocol-aware chronological blocks.
// Original capture bytes remain separately available in requests.
func auditConversationTranscript(rows []*service.UserRequestAudit) string {
	return auditMessagesTranscript(service.BuildUserRequestAuditTranscript(rows))
}

func auditMessagesTranscript(messages []service.UserRequestAuditMessage) string {
	var out strings.Builder
	for _, message := range messages {
		fmt.Fprintf(&out, "<|im_start|>%s\n%s<|im_end|>\n", message.Role, message.Content)
	}
	return out.String()
}
