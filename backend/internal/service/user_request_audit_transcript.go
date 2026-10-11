package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type UserRequestAuditMessage struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	Reasoning  string `json:"reasoning,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
	CallID     string `json:"call_id,omitempty"`
	Kind       string `json:"kind"`
	RequestID  int64  `json:"request_id"`
	Incomplete bool   `json:"incomplete,omitempty"`
}

type UserRequestAuditConversation struct {
	ID           int64                     `json:"id"`
	UserLabel    string                    `json:"user_label"`
	RequestCount int                       `json:"request_count"`
	Messages     []UserRequestAuditMessage `json:"messages"`
	Requests     []*UserRequestAudit       `json:"requests"`
}

type userRequestAuditConversationReader interface {
	GetConversation(context.Context, int64) (*UserRequestAuditConversation, error)
}

func (s *UserRequestAuditService) GetConversation(ctx context.Context, id int64) (*UserRequestAuditConversation, error) {
	if s == nil || s.repo == nil {
		return nil, sql.ErrNoRows
	}
	if repo, ok := s.repo.(userRequestAuditConversationReader); ok {
		return repo.GetConversation(ctx, id)
	}
	row, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return &UserRequestAuditConversation{ID: row.ID, UserLabel: row.UserLabel, RequestCount: 1, Requests: []*UserRequestAudit{row}, Messages: BuildUserRequestAuditTranscript([]*UserRequestAudit{row})}, nil
}

func auditMessageEqual(a, b UserRequestAuditMessage) bool {
	return a.Role == b.Role && a.Kind == b.Kind && a.Content == b.Content && a.Reasoning == b.Reasoning && a.ToolName == b.ToolName && a.CallID == b.CallID && !a.Incomplete && !b.Incomplete
}

// Client replay commonly omits private reasoning and protocol metadata. These
// blocks stay in the reader; matching uses only actual message/tool identity.
func auditReplayPrefix(request, history []UserRequestAuditMessage) int {
	i, j := 0, 0
	meaningful := false
	ignored := func(m UserRequestAuditMessage) bool {
		return m.Kind == "think" || m.Kind == "metadata" || m.Kind == "error"
	}
	for {
		for i < len(request) && ignored(request[i]) {
			// Do not erase newly supplied private reasoning merely because it is
			// excluded from ordinary replay comparison.
			if request[i].Kind == "think" {
				found := false
				for k := j; k < len(history) && ignored(history[k]); k++ {
					found = found || auditMessageEqual(request[i], history[k])
				}
				if !found {
					return 0
				}
			}
			i++
		}
		for j < len(history) && ignored(history[j]) {
			j++
		}
		if i == len(request) || j == len(history) {
			if meaningful {
				return i
			}
			return 0
		}
		if !auditMessageEqual(request[i], history[j]) {
			return 0
		}
		meaningful = meaningful || request[i].Role == "assistant" || request[i].Kind == "tool_result"
		i++
		j++
	}
}

// Only a complete prefix is replay evidence. A matching first question alone
// never removes a new turn, and status markers do not participate in matching.
func BuildUserRequestAuditTranscript(rows []*UserRequestAudit) []UserRequestAuditMessage {
	out := make([]UserRequestAuditMessage, 0)
	if len(rows) == 1 && rows[0].SessionID == "" && rows[0].PreviousResponseID == "" && rows[0].Protocol != ContentModerationProtocolTypeSafeSystemOne {
		out = append(out, UserRequestAuditMessage{Role: "system", Kind: "metadata", Content: "No stored session or previous-response link; only the captured request history is available.", RequestID: rows[0].ID})
	}
	var history []UserRequestAuditMessage
	var previousRequest []UserRequestAuditMessage
	previousRetryable := false
	for _, row := range rows {
		req := auditDecodeChatML(row.RequestChatML, "user", row.ID)
		response := auditDecodeChatML(row.ResponseChatML, "assistant", row.ID)
		if row.Protocol == ContentModerationProtocolTypeSafeSystemOne {
			req = nil
			response = nil
			if row.RequestChatML != "" {
				req = []UserRequestAuditMessage{{Role: "system", Kind: "metadata", Content: row.RequestChatML, RequestID: row.ID}}
			}
			if row.ResponseChatML != "" {
				response = []UserRequestAuditMessage{{Role: "system", Kind: "metadata", Content: row.ResponseChatML, RequestID: row.ID}}
			}
		}
		matched := auditReplayPrefix(req, history)
		// Exact resend of the previous full request is a retry, only inside an
		// already proven conversation. Otherwise require assistant/tool evidence.
		retry := previousRetryable && len(req) > 0 && len(req) == len(previousRequest)
		for i := 0; retry && i < len(req); i++ {
			retry = auditMessageEqual(req[i], previousRequest[i])
		}
		if retry {
			matched = len(req)
		}
		out = append(out, req[matched:]...)
		out = append(out, response...)
		// Rebase against the actual current full history, not errors/retries
		// already displayed. This preserves subsequent prefix comparison.
		if matched > 0 || len(history) == 0 {
			history = append(append([]UserRequestAuditMessage{}, req...), response...)
		} else {
			history = append(history, req...)
			history = append(history, response...)
		}
		previousRequest = req
		previousRetryable = row.Status != "completed"
		if strings.Contains(row.RequestChatML, "...[truncated]") || strings.Contains(row.ResponseChatML, "...[truncated]") {
			out = append(out, UserRequestAuditMessage{Role: "system", Kind: "metadata", Content: "Archive capture was truncated; omitted messages and THINK cannot be recovered.", RequestID: row.ID, Incomplete: true})
		}
		if len(req) == 0 {
			out = append(out, UserRequestAuditMessage{Role: "system", Kind: "metadata", Content: "Request content unavailable in this archive.", RequestID: row.ID, Incomplete: true})
		}
		if len(response) == 0 {
			out = append(out, UserRequestAuditMessage{Role: "system", Kind: "metadata", Content: "Response content unavailable in this archive; no plaintext THINK can be recovered.", RequestID: row.ID, Incomplete: true})
		}
		if row.Status != "completed" || row.LastError != "" {
			content := "Request status: " + row.Status
			if row.LastError != "" {
				content += "\n" + row.LastError
			}
			out = append(out, UserRequestAuditMessage{Role: "system", Kind: "error", Content: content, RequestID: row.ID, Incomplete: true})
		}
		keys := make([]string, 0, len(row.Metadata))
		for key := range row.Metadata {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			value := row.Metadata[key]
			if strings.Contains(key, "truncat") && value != false && value != nil && value != "" {
				out = append(out, UserRequestAuditMessage{Role: "system", Kind: "metadata", Content: fmt.Sprintf("Captured content boundary: %s=%v", key, value), RequestID: row.ID, Incomplete: true})
			}
		}
	}
	return out
}

// Prefix proof is indexed from captured payloads, never caller-provided hashes.
// Hashes cover all message roles, tool identities and exact text, not a prompt.
type UserRequestAuditContinuityProof struct {
	Prefixes []string
	Terminal string
}

func UserRequestAuditProof(row *UserRequestAudit) UserRequestAuditContinuityProof {
	proof := UserRequestAuditContinuityProof{}
	if strings.Contains(row.RequestChatML, "...[truncated]") || strings.Contains(row.ResponseChatML, "...[truncated]") || row.Protocol == ContentModerationProtocolTypeSafeSystemOne {
		return proof
	}
	for key, value := range row.Metadata {
		if strings.Contains(key, "truncat") && value != false && value != nil && value != "" {
			return proof
		}
	}
	req := auditDecodeChatML(row.RequestChatML, "user", 0)
	res := auditDecodeChatML(row.ResponseChatML, "assistant", 0)
	h := sha256.New()
	meaningful := false
	valid := true
	add := func(m UserRequestAuditMessage) string {
		m.RequestID = 0
		if m.Incomplete || m.Kind == "error" || (m.Kind == "metadata" && m.Role != "tools" && m.Role != "reasoning" && m.Role != "system") {
			valid = false
		}
		meaningful = meaningful || m.Role == "assistant" || m.Kind == "tool_result"
		b, _ := json.Marshal(m)
		h.Write(b)
		h.Write([]byte{0})
		return hex.EncodeToString(h.Sum(nil))
	}
	for i, m := range req {
		key := add(m)
		if valid && meaningful && i >= 1 && i < len(req)-1 {
			proof.Prefixes = append(proof.Prefixes, key)
		}
	}
	for _, m := range res {
		add(m)
	}
	if valid && meaningful && len(req)+len(res) >= 2 && (row.Status == "completed" || len(res) == 0) {
		proof.Terminal = hex.EncodeToString(h.Sum(nil))
	}
	return proof
}

func auditJSONText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}
func auditString(m map[string]any, key string) string { s, _ := m[key].(string); return s }

func auditDecodeChatML(raw, role string, id int64) []UserRequestAuditMessage {
	if raw == "" {
		return nil
	}
	if strings.HasPrefix(raw, "<|assistant|>\n") {
		return auditDecodeWire(strings.TrimPrefix(raw, "<|assistant|>\n"), id)
	}
	var out []UserRequestAuditMessage
	for raw != "" {
		start := strings.Index(raw, "<|im_start|>")
		if start < 0 {
			if strings.TrimSpace(raw) != "" {
				out = append(out, auditDecodeValue(raw, role, id)...)
			}
			break
		}
		if strings.TrimSpace(raw[:start]) != "" {
			out = append(out, UserRequestAuditMessage{Role: "system", Kind: "metadata", Content: raw[:start], RequestID: id})
		}
		raw = raw[start+len("<|im_start|>"):]
		nl := strings.IndexByte(raw, '\n')
		if nl < 0 {
			out = append(out, UserRequestAuditMessage{Role: "system", Kind: "metadata", Content: raw, RequestID: id, Incomplete: true})
			break
		}
		blockRole := raw[:nl]
		raw = raw[nl+1:]
		end := strings.Index(raw, "<|im_end|>")
		if end < 0 {
			msgs := auditDecodeValue(raw, blockRole, id)
			for i := range msgs {
				msgs[i].Incomplete = true
			}
			out = append(out, msgs...)
			break
		}
		out = append(out, auditDecodeValue(raw[:end], blockRole, id)...)
		raw = strings.TrimPrefix(raw[end+len("<|im_end|>"):], "\n")
	}
	return out
}

func auditDecodeValue(raw, role string, id int64) []UserRequestAuditMessage {
	if role == "reasoning" || role == "tools" {
		return []UserRequestAuditMessage{{Role: role, Kind: "metadata", Content: raw, RequestID: id}}
	}
	var v any
	if json.Unmarshal([]byte(raw), &v) == nil {
		switch v.(type) {
		case map[string]any, []any:
			return auditDecodeObject(v, role, id)
		}
	}
	if role != "assistant" {
		kind := "text"
		if role == "tool" {
			kind = "tool_result"
		}
		return []UserRequestAuditMessage{{Role: role, Kind: kind, Content: raw, RequestID: id, Incomplete: strings.Contains(raw, "[truncated]")}}
	}
	var out []UserRequestAuditMessage
	for raw != "" {
		i := strings.Index(raw, "<think>")
		if i < 0 {
			out = append(out, UserRequestAuditMessage{Role: role, Kind: "text", Content: raw, RequestID: id})
			break
		}
		if i > 0 {
			out = append(out, UserRequestAuditMessage{Role: role, Kind: "text", Content: raw[:i], RequestID: id})
		}
		raw = raw[i+7:]
		end := strings.Index(raw, "</think>")
		if end < 0 {
			out = append(out, UserRequestAuditMessage{Role: role, Kind: "think", Content: raw, Reasoning: raw, RequestID: id, Incomplete: true})
			break
		}
		out = append(out, UserRequestAuditMessage{Role: role, Kind: "think", Content: raw[:end], Reasoning: raw[:end], RequestID: id})
		raw = raw[end+8:]
	}
	return out
}

func auditDecodeObject(v any, role string, id int64) []UserRequestAuditMessage {
	if v == nil {
		return nil
	}
	if a, ok := v.([]any); ok {
		var out []UserRequestAuditMessage
		for _, p := range a {
			out = append(out, auditDecodeObject(p, role, id)...)
		}
		return out
	}
	if s, ok := v.(string); ok {
		return auditDecodeValueText(s, role, id)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return []UserRequestAuditMessage{{Role: role, Kind: "metadata", Content: auditJSONText(v), RequestID: id}}
	}
	if r := auditString(m, "role"); r != "" {
		role = r
	}
	typ := auditString(m, "type")
	block := func(kind, text, name, call string) []UserRequestAuditMessage {
		msg := UserRequestAuditMessage{Role: role, Kind: kind, Content: text, ToolName: name, CallID: call, RequestID: id}
		if kind == "think" {
			msg.Role = "assistant"
			msg.Reasoning = text
		}
		return []UserRequestAuditMessage{msg}
	}
	switch typ {
	case "function_call", "tool_use":
		args := m["arguments"]
		if typ == "tool_use" {
			args = m["input"]
		}
		call := auditString(m, "call_id")
		if call == "" {
			call = auditString(m, "id")
		}
		role = "assistant"
		return block("tool_call", auditJSONText(args), auditString(m, "name"), call)
	case "function_call_output", "tool_result":
		value := m["output"]
		call := auditString(m, "call_id")
		if typ == "tool_result" {
			value = m["content"]
			call = auditString(m, "tool_use_id")
		}
		role = "tool"
		return block("tool_result", auditJSONText(value), "", call)
	case "thinking", "reasoning_text", "summary_text":
		text := auditString(m, "thinking")
		if text == "" {
			text = auditString(m, "text")
		}
		return block("think", text, "", "")
	case "reasoning":
		var out []UserRequestAuditMessage
		for _, k := range []string{"summary", "content"} {
			if m[k] != nil {
				out = append(out, auditDecodeObject(m[k], "assistant", id)...)
			}
		}
		if len(out) == 0 {
			return block("metadata", "Reasoning item has no captured plaintext (encrypted or unavailable).", "", "")
		}
		return out
	case "redacted_thinking":
		return block("metadata", "Encrypted/redacted thinking; plaintext unavailable.", "", "")
	case "text", "input_text", "output_text":
		return auditDecodeValueText(auditString(m, "text"), role, id)
	}
	if choices, ok := m["choices"].([]any); ok {
		var out []UserRequestAuditMessage
		for _, c := range choices {
			if choice, ok := c.(map[string]any); ok {
				value := choice["message"]
				if value == nil {
					value = choice["delta"]
				}
				out = append(out, auditDecodeObject(value, "assistant", id)...)
			}
		}
		return out
	}
	if output, ok := m["output"].([]any); ok {
		return auditDecodeObject(output, "assistant", id)
	}
	var out []UserRequestAuditMessage
	if reasoning := auditString(m, "reasoning_content"); reasoning != "" {
		out = append(out, block("think", reasoning, "", "")...)
	}
	if content, exists := m["content"]; exists {
		if role == "tool" {
			out = append(out, block("tool_result", auditJSONText(content), auditString(m, "name"), auditString(m, "tool_call_id"))...)
		} else {
			out = append(out, auditDecodeObject(content, role, id)...)
		}
	}
	if calls, ok := m["tool_calls"].([]any); ok {
		for _, c := range calls {
			call, ok := c.(map[string]any)
			if !ok {
				continue
			}
			f, _ := call["function"].(map[string]any)
			role = "assistant"
			out = append(out, block("tool_call", auditString(f, "arguments"), auditString(f, "name"), auditString(call, "id"))...)
		}
	}
	if len(out) > 0 {
		return out
	}
	return block("metadata", auditJSONText(m), "", "")
}

func auditDecodeValueText(text, role string, id int64) []UserRequestAuditMessage {
	if text == "" {
		return nil
	}
	if role == "assistant" && strings.Contains(text, "<think>") {
		return auditDecodeValue(text, role, id)
	}
	kind := "text"
	if role == "tool" {
		kind = "tool_result"
	}
	return []UserRequestAuditMessage{{Role: role, Kind: kind, Content: text, RequestID: id}}
}
