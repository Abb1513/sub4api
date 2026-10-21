package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/anthropictokenizer"
	"github.com/MACOS-DO/sub4api/internal/pkg/apicompat"
	kiropkg "github.com/MACOS-DO/sub4api/internal/pkg/kiro"
)

const (
	kiroCacheDefaultTTL       = 5 * time.Minute
	kiroCacheOneHourTTL       = time.Hour
	kiroCacheMaxSupportedTTL  = time.Hour
	kiroTokensPerTool         = 150
	kiroTokensPerMessage      = 4
	kiroCacheMinTokensDefault = 1024
	kiroCacheMinTokensOpus    = 4096
	// kiroCacheMinTokensGPT 与 default 同值但语义独立：1024 对齐 OpenAI 官方的最小
	// 缓存粒度，不应随 default 一起调整。见 kiroMinimumCacheableTokens。
	kiroCacheMinTokensGPT        = 1024
	kiroCachePrefixLookbackLimit = 10
	kiroCacheNamespaceCapacity   = 4096
	kiroCacheGlobalCapacity      = 65536
	kiroCacheFullPruneInterval   = 30 * time.Second
)

type kiroCacheEmulationUsage struct {
	InputTokens                int
	CacheReadInputTokens       int
	CacheCreationInputTokens   int
	CacheCreation5mInputTokens int
	CacheCreation1hInputTokens int
}

type kiroCacheEntry struct {
	tokens    int
	ttl       time.Duration
	expiresAt time.Time
	lastHitAt time.Time
}

type kiroCacheTracker struct {
	mu             sync.Mutex
	entries        map[uint64]map[[32]byte]kiroCacheEntry
	entryCount     int
	capacity       int
	globalCapacity int
	lastFullPrune  time.Time
}

func newKiroCacheTracker() *kiroCacheTracker {
	return &kiroCacheTracker{
		entries:        make(map[uint64]map[[32]byte]kiroCacheEntry),
		capacity:       kiroCacheNamespaceCapacity,
		globalCapacity: kiroCacheGlobalCapacity,
	}
}

var globalKiroCacheTracker = newKiroCacheTracker()

type kiroPromptCacheIsolationInput struct {
	SessionID string
	APIKeyID  int64
}

// kiroCacheEmulationPlan 把缓存估算拆成"计算"与"落盘"两步：prepare 阶段只读 tracker
// 得到估算结果，commit() 才会把本次前缀写入 tracker。调用方应在确认上游请求成功后
// 再 commit()，避免请求失败/未发出时就把内容错误标记为已缓存，污染下一次请求的估算。
type kiroCacheEmulationPlan struct {
	usage    *kiroCacheEmulationUsage
	cacheKey uint64
	profile  *kiroCacheProfile
}

func (p *kiroCacheEmulationPlan) result() *kiroCacheEmulationUsage {
	if p == nil {
		return nil
	}
	return p.usage
}

func (p *kiroCacheEmulationPlan) commit() {
	if p == nil || p.profile == nil || p.cacheKey == 0 {
		return
	}
	globalKiroCacheTracker.update(p.cacheKey, p.profile)
}

func (s *GatewayService) buildKiroCacheEmulationUsage(ctx context.Context, account *Account, group *Group, body []byte, model string, inputTokens int) *kiroCacheEmulationUsage {
	return s.buildKiroCacheEmulationUsageForParsed(ctx, account, group, nil, body, model, inputTokens)
}

func (s *GatewayService) buildKiroCacheEmulationUsageForParsed(ctx context.Context, account *Account, group *Group, parsed *ParsedRequest, body []byte, model string, inputTokens int) *kiroCacheEmulationUsage {
	plan := s.prepareKiroCacheEmulationUsageForParsed(ctx, account, group, parsed, body, model, inputTokens)
	plan.commit()
	return plan.result()
}

func (s *GatewayService) prepareKiroCacheEmulationUsage(ctx context.Context, account *Account, group *Group, body []byte, model string, inputTokens int) *kiroCacheEmulationPlan {
	return s.prepareKiroCacheEmulationUsageForParsed(ctx, account, group, nil, body, model, inputTokens)
}

func (s *GatewayService) prepareKiroCacheEmulationUsageForParsed(ctx context.Context, account *Account, group *Group, parsed *ParsedRequest, body []byte, model string, inputTokens int) *kiroCacheEmulationPlan {
	NormalizeGroupRuntimeFields(group)
	if group == nil || !group.EffectiveKiroCacheEmulationEnabled() || account == nil || account.ID <= 0 || len(body) == 0 {
		return nil
	}
	profile, ok := buildKiroCacheProfileForParsed(ctx, s.kiroMeteringAlgorithm(ctx), parsed, body, model, inputTokens)
	if !ok {
		return nil
	}
	return s.prepareKiroCacheEmulationPlanFromProfile(account, group, parsed, profile, inputTokens)
}

func (s *GatewayService) buildKiroResponsesCacheEmulationUsage(ctx context.Context, account *Account, group *Group, body []byte, model string, inputTokens int) *kiroCacheEmulationUsage {
	return s.buildKiroResponsesCacheEmulationUsageForParsed(ctx, account, group, nil, body, model, inputTokens)
}

func (s *GatewayService) buildKiroResponsesCacheEmulationUsageForParsed(ctx context.Context, account *Account, group *Group, parsed *ParsedRequest, body []byte, model string, inputTokens int) *kiroCacheEmulationUsage {
	plan := s.prepareKiroResponsesCacheEmulationUsageForParsed(ctx, account, group, parsed, body, model, inputTokens)
	plan.commit()
	return plan.result()
}

func (s *GatewayService) prepareKiroResponsesCacheEmulationUsageForParsed(ctx context.Context, account *Account, group *Group, parsed *ParsedRequest, body []byte, model string, inputTokens int) *kiroCacheEmulationPlan {
	NormalizeGroupRuntimeFields(group)
	if group == nil || !group.EffectiveKiroCacheEmulationEnabled() || account == nil || account.ID <= 0 || len(body) == 0 {
		return nil
	}
	profile, ok := buildKiroResponsesCacheProfileForParsed(ctx, s.kiroMeteringAlgorithm(ctx), parsed, body, model, inputTokens)
	if !ok {
		return nil
	}
	return s.prepareKiroCacheEmulationPlanFromProfile(account, group, parsed, profile, inputTokens)
}

func (s *GatewayService) buildKiroChatCompletionsCacheEmulationUsage(ctx context.Context, account *Account, group *Group, body []byte, model string, inputTokens int) *kiroCacheEmulationUsage {
	return s.buildKiroChatCompletionsCacheEmulationUsageForParsed(ctx, account, group, nil, body, model, inputTokens)
}

func (s *GatewayService) buildKiroChatCompletionsCacheEmulationUsageForParsed(ctx context.Context, account *Account, group *Group, parsed *ParsedRequest, body []byte, model string, inputTokens int) *kiroCacheEmulationUsage {
	plan := s.prepareKiroChatCompletionsCacheEmulationUsageForParsed(ctx, account, group, parsed, body, model, inputTokens)
	plan.commit()
	return plan.result()
}

func (s *GatewayService) prepareKiroChatCompletionsCacheEmulationUsageForParsed(ctx context.Context, account *Account, group *Group, parsed *ParsedRequest, body []byte, model string, inputTokens int) *kiroCacheEmulationPlan {
	NormalizeGroupRuntimeFields(group)
	if group == nil || !group.EffectiveKiroCacheEmulationEnabled() || account == nil || account.ID <= 0 || len(body) == 0 {
		return nil
	}
	profile, ok := buildKiroChatCompletionsCacheProfileForParsed(ctx, s.kiroMeteringAlgorithm(ctx), parsed, body, model, inputTokens)
	if !ok {
		return nil
	}
	effectiveInputTokens := inputTokens
	if effectiveInputTokens <= 0 {
		effectiveInputTokens = profile.totalInputTokens
	}
	return s.prepareKiroCacheEmulationPlanFromProfile(account, group, parsed, profile, effectiveInputTokens)
}

func (s *GatewayService) prepareKiroCacheEmulationPlanFromProfile(account *Account, group *Group, parsed *ParsedRequest, profile *kiroCacheProfile, inputTokens int) *kiroCacheEmulationPlan {
	if group == nil || account == nil || account.ID <= 0 || profile == nil {
		return nil
	}
	cacheKey := kiroCacheCredentialKey(account, parsed, profile.model)
	if cacheKey == 0 {
		return nil
	}
	creationRatio, readRatio := group.EffectiveKiroCacheEmulationRatios()
	result := globalKiroCacheTracker.compute(cacheKey, profile, inputTokens, creationRatio, readRatio)
	if result == nil {
		return nil
	}
	if result.CacheReadInputTokens == 0 && result.CacheCreationInputTokens == 0 {
		result = nil
	}
	if result == nil {
		return nil
	}
	return &kiroCacheEmulationPlan{usage: result, cacheKey: cacheKey, profile: profile}
}

func scaleKiroCacheCreationTTLTokens(tokens5m, tokens1h, scaledTotal int, ratio float64) (int, int) {
	if scaledTotal <= 0 || ratio <= 0 {
		return 0, 0
	}
	if tokens5m <= 0 && tokens1h <= 0 {
		return 0, 0
	}
	if tokens1h <= 0 {
		return scaledTotal, 0
	}
	if tokens5m <= 0 {
		return 0, scaledTotal
	}
	scaled5m := scaleKiroCacheTokens(tokens5m, ratio)
	if scaled5m > scaledTotal {
		scaled5m = scaledTotal
	}
	scaled1h := scaledTotal - scaled5m
	return scaled5m, scaled1h
}

func scaleKiroCacheTokens(tokens int, ratio float64) int {
	if tokens <= 0 || ratio <= 0 {
		return 0
	}
	if ratio >= 1 {
		return tokens
	}
	return int(math.Round(float64(tokens) * ratio))
}

type kiroCacheProfile struct {
	totalInputTokens int
	profileTotalEst  int
	minCacheable     int
	model            string
	blocks           []kiroCacheBlock
	breakpoints      []kiroCacheBreakpoint
}

type kiroCacheBlock struct {
	prefixFingerprint [32]byte
	cumulativeTokens  int
}

type kiroCacheBreakpoint struct {
	blockIndex int
	ttl        time.Duration
}

type kiroResolvedBreakpoint struct {
	blockIndex       int
	cumulativeTokens int
	ttl              time.Duration
}

type kiroPendingBlock struct {
	value         any
	tokens        int
	breakpointTTL *time.Duration
	messageIndex  *int
	isMessageEnd  bool
	skipPrefix    bool
}

func buildKiroCacheProfileForParsed(ctx context.Context, alg anthropictokenizer.Algorithm, parsed *ParsedRequest, body []byte, model string, inputTokens int) (*kiroCacheProfile, bool) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false
	}
	blocks := flattenKiroCacheBlocks(ctx, alg, payload)
	if len(blocks) == 0 {
		return nil, false
	}
	// 客户端未下发任何 cache_control 时兜底补断点。
	//
	// Anthropic 路径的断点原本完全依赖客户端：无 cache_control ⇒ 无断点 ⇒
	// lastCacheableBreakpoint 为 nil ⇒ buildKiroCacheProfileFromBlocks 返回 false ⇒ usage
	// 为 nil，该请求零 cache_read 却仍计入命中率分母。Claude Code 会下发，但 curl、
	// Cherry Studio 等 OpenAI 风格客户端普遍不发，这批流量是纯粹的分母污染。
	//
	// 只在「完全没有客户端断点」时兜底，不与客户端断点混用：若客户端已标注，额外补断点等于
	// 在真实 API 不会缓存的位置模拟出 cache_read，会高估命中率、少计费。
	hasClientBreakpoints := hasKiroClientCacheBreakpoint(blocks)
	if !hasClientBreakpoints {
		applyKiroDefaultBreakpoints(blocks, kiroCacheDefaultTTL)
	}
	totalTokens := inputTokens
	if totalTokens <= 0 {
		totalTokens = countKiroInputTokensFromPayload(ctx, alg, payload)
	}
	prelude := map[string]any{
		"model":       payload["model"],
		"tool_choice": payload["tool_choice"],
	}
	return buildKiroCacheProfileFromBlocks(model, totalTokens, kiroPromptCacheIsolationForParsed(parsed), prelude, blocks, hasClientBreakpoints)
}

func buildKiroResponsesCacheProfileForParsed(ctx context.Context, alg anthropictokenizer.Algorithm, parsed *ParsedRequest, body []byte, model string, inputTokens int) (*kiroCacheProfile, bool) {
	var req apicompat.ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, false
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false
	}
	blocks := flattenKiroResponsesCacheBlocks(ctx, alg, payload)
	if len(blocks) == 0 {
		return nil, false
	}
	ttl := kiroCacheDefaultTTL
	applyKiroDefaultBreakpoints(blocks, ttl)

	effectiveTools, err := apicompat.EffectiveResponsesTools(&req)
	if err != nil {
		return nil, false
	}
	effectiveModel := strings.TrimSpace(model)
	if effectiveModel == "" {
		effectiveModel, _ = payload["model"].(string)
	}
	totalTokens := inputTokens
	if totalTokens <= 0 {
		totalTokens = countKiroResponsesInputTokens(payload, blocks, len(effectiveTools))
	}
	prelude := map[string]any{
		"protocol":             "responses",
		"model":                effectiveModel,
		"tool_choice":          payload["tool_choice"],
		"tools":                kiroJSONCompatibleValue(effectiveTools),
		"prompt_cache_key":     payload["prompt_cache_key"],
		"previous_response_id": payload["previous_response_id"],
		"reasoning_effort":     kiroNestedValue(payload, "reasoning", "effort"),
		"text_format":          kiroNestedValue(payload, "text", "format"),
	}
	return buildKiroCacheProfileFromBlocks(effectiveModel, totalTokens, kiroPromptCacheIsolationForParsed(parsed), prelude, blocks, false)
}

func buildKiroChatCompletionsCacheProfileForParsed(ctx context.Context, alg anthropictokenizer.Algorithm, parsed *ParsedRequest, body []byte, model string, inputTokens int) (*kiroCacheProfile, bool) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false
	}
	blocks := flattenKiroChatCompletionsCacheBlocks(ctx, alg, payload)
	if len(blocks) == 0 {
		return nil, false
	}
	applyKiroDefaultBreakpoints(blocks, kiroCacheDefaultTTL)

	effectiveModel := strings.TrimSpace(model)
	if effectiveModel == "" {
		effectiveModel, _ = payload["model"].(string)
	}
	tools, _ := payload["tools"].([]any)
	functions, _ := payload["functions"].([]any)
	totalTokens := inputTokens
	if totalTokens <= 0 {
		totalTokens = countKiroChatCompletionsInputTokens(payload, blocks, len(tools)+len(functions))
	}
	prelude := map[string]any{
		"protocol":            "chat_completions",
		"model":               effectiveModel,
		"instructions":        payload["instructions"],
		"tool_choice":         payload["tool_choice"],
		"function_call":       payload["function_call"],
		"tools":               kiroJSONCompatibleValue(payload["tools"]),
		"functions":           kiroJSONCompatibleValue(payload["functions"]),
		"parallel_tool_calls": payload["parallel_tool_calls"],
		"reasoning_effort":    payload["reasoning_effort"],
		"response_format":     kiroJSONCompatibleValue(payload["response_format"]),
	}
	return buildKiroCacheProfileFromBlocks(effectiveModel, totalTokens, kiroPromptCacheIsolationForParsed(parsed), prelude, blocks, false)
}

func buildKiroCacheProfileFromBlocks(model string, totalTokens int, isolation kiroPromptCacheIsolationInput, preludeValue any, blocks []kiroPendingBlock, propagateMessageBreakpoints bool) (*kiroCacheProfile, bool) {
	prelude, err := canonicalJSON(preludeValue)
	if err != nil {
		return nil, false
	}
	seed := "account"
	switch {
	case isolation.SessionID != "":
		seed = "session:" + isolation.SessionID
	case isolation.APIKeyID > 0:
		seed = "api_key:" + fmt.Sprint(isolation.APIKeyID)
	}
	isolationJSON := []byte(seed)
	modelJSON, err := canonicalJSON(model)
	if err != nil {
		return nil, false
	}
	prefixState := make([]byte, 0, 24+len(isolationJSON)+len(modelJSON)+len(prelude))
	var length [8]byte
	for _, chunk := range [][]byte{isolationJSON, modelJSON, prelude} {
		binary.BigEndian.PutUint64(length[:], uint64(len(chunk)))
		prefixState = append(prefixState, length[:]...)
		prefixState = append(prefixState, chunk...)
	}

	profile := &kiroCacheProfile{
		totalInputTokens: max(totalTokens, 0),
		profileTotalEst:  0,
		minCacheable:     kiroMinimumCacheableTokens(model),
		model:            model,
	}
	cumulativeTokens := 0
	var activeTTL *time.Duration
	seenBreakpoints := make(map[int]struct{})
	for index, block := range blocks {
		profile.profileTotalEst += max(block.tokens, 0)
		if block.skipPrefix {
			// Keep the block position aligned with kiroPendingBlock indices so
			// breakpoints after a dynamic system head still resolve correctly.
			profile.blocks = append(profile.blocks, kiroCacheBlock{cumulativeTokens: cumulativeTokens})
			continue
		}
		cumulativeTokens += max(block.tokens, 0)
		blockJSON, err := canonicalJSON(block.value)
		if err != nil {
			return nil, false
		}
		blockHash := sha256.Sum256(blockJSON)
		h := sha256.New()
		_, _ = h.Write(prefixState)
		_, _ = h.Write(blockHash[:])
		prefixFingerprint := [32]byte(h.Sum(nil))
		prefixState = prefixFingerprint[:]
		profile.blocks = append(profile.blocks, kiroCacheBlock{prefixFingerprint: prefixFingerprint, cumulativeTokens: cumulativeTokens})

		if block.breakpointTTL != nil {
			ttl := minDuration(*block.breakpointTTL, kiroCacheMaxSupportedTTL)
			activeTTL = &ttl
			if _, ok := seenBreakpoints[index]; !ok {
				profile.breakpoints = append(profile.breakpoints, kiroCacheBreakpoint{blockIndex: index, ttl: ttl})
				seenBreakpoints[index] = struct{}{}
			}
		}
		if propagateMessageBreakpoints && block.isMessageEnd && block.messageIndex != nil && activeTTL != nil {
			if _, ok := seenBreakpoints[index]; !ok {
				profile.breakpoints = append(profile.breakpoints, kiroCacheBreakpoint{blockIndex: index, ttl: *activeTTL})
				seenBreakpoints[index] = struct{}{}
			}
		}
	}
	if profile.lastBreakpoint() == nil {
		return nil, false
	}
	return profile, true
}

func kiroPromptCacheIsolationForParsed(parsed *ParsedRequest) kiroPromptCacheIsolationInput {
	if parsed == nil {
		return kiroPromptCacheIsolationInput{}
	}
	if sessionID, ok := extractKiroPromptCacheSessionID(parsed.MetadataUserID); ok {
		return kiroPromptCacheIsolationInput{SessionID: sessionID}
	}
	if parsed.SessionContext != nil && parsed.SessionContext.APIKeyID > 0 {
		return kiroPromptCacheIsolationInput{APIKeyID: parsed.SessionContext.APIKeyID}
	}
	return kiroPromptCacheIsolationInput{}
}

// extractKiroPromptCacheSessionID mirrors Kiro-Go's Claude Code metadata parsing.
// It accepts either JSON {"session_id":"..."} or the
// user_<hash>_account__session_<id> form.
func extractKiroPromptCacheSessionID(userID string) (string, bool) {
	trimmed := strings.TrimSpace(userID)
	if trimmed == "" {
		return "", false
	}
	if strings.HasPrefix(trimmed, "{") {
		var decoded map[string]any
		if json.Unmarshal([]byte(trimmed), &decoded) == nil {
			if sessionID, ok := decoded["session_id"].(string); ok {
				if sessionID = strings.TrimSpace(sessionID); sessionID != "" {
					return sessionID, true
				}
			}
		}
	}
	if _, after, found := strings.Cut(trimmed, "_session_"); found {
		if sessionID := strings.TrimSpace(after); sessionID != "" {
			return sessionID, true
		}
	}
	return "", false
}

func flattenKiroCacheBlocks(ctx context.Context, alg anthropictokenizer.Algorithm, payload map[string]any) []kiroPendingBlock {
	var blocks []kiroPendingBlock
	if tools, ok := payload["tools"].([]any); ok {
		for toolIndex, tool := range tools {
			value := stripKiroCacheControl(tool)
			blocks = append(blocks, kiroPendingBlock{
				value: map[string]any{"kind": "tool", "tool_index": toolIndex, "tool": value},
				// 工具块按序列化后的真实 token 计数，不用 kiroTokensPerTool 拍平值。
				//
				// 真实 Claude Code 工具 schema 单个 300~1500 token，15 个工具实测约 21000，
				// 而拍平估算只给 15*150=2250，低估约 9 倍。后果不是「命中率略低」而是
				// 「整个 profile 被拒」：cacheableBreakpoints 用累计值与 minCacheable 比较，
				// opus 系阈值 4096（见 kiroMinimumCacheableTokens，该值是被
				// TestKiroMinimumCacheableTokens 钉死的显式契约，不应为此调低）。带 tools 的
				// opus 短请求累计值卡在 2000 上下 → lastCacheableBreakpoint 为 nil →
				// buildKiroCacheProfileFromBlocks 返回 false → 该请求零缓存，却仍然计入
				// 命中率分母。
				//
				// 只改这里（B 侧）不动 countKiroInputTokensFromPayload 等计数器（T 侧）：
				// P1 归一化后命中率 = B_matched/B_last，T 被约掉，所以修正 B 即可解决问题；
				// 而 T 还喂给计费兜底与 count_tokens 对外契约，改动会牵连账单。两侧工具口径
				// 因此不一致，但归一化把 B_last 精确映射到 T，该差异不会外泄。
				tokens:        countKiroSerializedValueTokens(alg, value),
				breakpointTTL: extractKiroCacheTTL(tool),
			})
		}
	}
	systemBlocks := normalizeKiroSystemBlocks(payload["system"])
	firstSystemBreakpoint := len(systemBlocks)
	for systemIndex, systemBlock := range systemBlocks {
		if extractKiroCacheTTL(systemBlock) != nil {
			firstSystemBreakpoint = systemIndex
			break
		}
	}
	for systemIndex, systemBlock := range systemBlocks {
		value := stripKiroCacheControl(systemBlock)
		canonicalizeKiroSystemBlock(value)
		blocks = append(blocks, kiroPendingBlock{
			value:         map[string]any{"kind": "system", "system_index": systemIndex, "block": value},
			tokens:        countKiroSystemBlockTokens(alg, systemBlock),
			skipPrefix:    firstSystemBreakpoint < len(systemBlocks) && systemIndex < firstSystemBreakpoint,
			breakpointTTL: extractKiroCacheTTL(systemBlock),
		})
	}
	messages, _ := payload["messages"].([]any)
	for messageIndex, rawMessage := range messages {
		message, _ := rawMessage.(map[string]any)
		role, _ := message["role"].(string)
		content := message["content"]
		switch typed := content.(type) {
		case string:
			mi := messageIndex
			block := map[string]any{"type": "text", "text": typed}
			blocks = append(blocks, kiroPendingBlock{
				value:  map[string]any{"kind": "message", "message_index": messageIndex, "role": role, "block_index": 0, "block": block},
				tokens: countKiroMessageContentTokens(ctx, alg, block), messageIndex: &mi, isMessageEnd: true,
			})
		case []any:
			lastBlockIndex := len(typed) - 1
			for blockIndex, rawBlock := range typed {
				mi := messageIndex
				value := stripKiroCacheControl(rawBlock)
				if text, ok := rawBlock.(string); ok {
					value = map[string]any{"type": "text", "text": text}
				}
				blocks = append(blocks, kiroPendingBlock{
					value:  map[string]any{"kind": "message", "message_index": messageIndex, "role": role, "block_index": blockIndex, "block": value},
					tokens: countKiroMessageContentTokens(ctx, alg, rawBlock), breakpointTTL: extractKiroCacheTTL(rawBlock), messageIndex: &mi, isMessageEnd: blockIndex == lastBlockIndex,
				})
			}
		}
	}
	return blocks
}

func flattenKiroResponsesCacheBlocks(ctx context.Context, alg anthropictokenizer.Algorithm, payload map[string]any) []kiroPendingBlock {
	var blocks []kiroPendingBlock
	if tools, ok := payload["tools"].([]any); ok {
		for toolIndex, tool := range tools {
			value := stripKiroCacheControl(tool)
			blocks = append(blocks, kiroPendingBlock{
				value:  map[string]any{"kind": "tool", "tool_index": toolIndex, "tool": value},
				tokens: countKiroSerializedValueTokens(alg, value),
			})
		}
	}
	if instructions, ok := payload["instructions"].(string); strings.TrimSpace(instructions) != "" && ok {
		block := map[string]any{"type": "input_text", "text": strings.TrimSpace(instructions)}
		blocks = append(blocks, kiroPendingBlock{
			value:        map[string]any{"kind": "instructions", "block": block},
			tokens:       countKiroMessageContentTokens(ctx, alg, block),
			isMessageEnd: true,
		})
	}

	switch input := payload["input"].(type) {
	case string:
		mi := 0
		block := map[string]any{"type": "input_text", "text": input}
		blocks = append(blocks, kiroPendingBlock{
			value:        map[string]any{"kind": "input", "input_index": 0, "role": "user", "block_index": 0, "block": block},
			tokens:       countKiroMessageContentTokens(ctx, alg, block),
			messageIndex: &mi,
			isMessageEnd: true,
		})
	case []any:
		cacheableItemCount := countKiroResponsesCacheableInputItems(input)
		seenCacheableItems := 0
		for itemIndex, rawItem := range input {
			if !isKiroResponsesCacheableInputItem(rawItem) {
				blocks = appendKiroResponsesInputItemBlocks(ctx, alg, blocks, itemIndex, rawItem, false)
				continue
			}
			seenCacheableItems++
			isStableHistoryItem := seenCacheableItems < cacheableItemCount
			blocks = appendKiroResponsesInputItemBlocks(ctx, alg, blocks, itemIndex, rawItem, isStableHistoryItem)
		}
	}
	return blocks
}

func flattenKiroChatCompletionsCacheBlocks(ctx context.Context, alg anthropictokenizer.Algorithm, payload map[string]any) []kiroPendingBlock {
	var blocks []kiroPendingBlock
	for _, key := range []string{"tools", "functions"} {
		values, _ := payload[key].([]any)
		for index, tool := range values {
			value := stripKiroCacheControl(tool)
			blocks = append(blocks, kiroPendingBlock{
				value:  map[string]any{"kind": key, "tool_index": index, "tool": value},
				tokens: countKiroSerializedValueTokens(alg, value),
			})
		}
	}
	if instructions, ok := payload["instructions"].(string); ok && strings.TrimSpace(instructions) != "" {
		block := map[string]any{"type": "input_text", "text": strings.TrimSpace(instructions)}
		blocks = append(blocks, kiroPendingBlock{
			value:  map[string]any{"kind": "instructions", "block": block},
			tokens: countKiroMessageContentTokens(ctx, alg, block),
		})
	}

	messages, _ := payload["messages"].([]any)
	for messageIndex, rawMessage := range messages {
		markMessageEnd := isKiroChatCompletionsCacheableMessage(rawMessage)
		blocks = appendKiroChatCompletionsMessageBlocks(ctx, alg, blocks, messageIndex, rawMessage, markMessageEnd)
	}
	return blocks
}

func isKiroChatCompletionsCacheableMessage(rawMessage any) bool {
	message, ok := rawMessage.(map[string]any)
	if !ok {
		return false
	}
	role, _ := message["role"].(string)
	return strings.TrimSpace(role) != ""
}

func appendKiroChatCompletionsMessageBlocks(ctx context.Context, alg anthropictokenizer.Algorithm, blocks []kiroPendingBlock, messageIndex int, rawMessage any, markMessageEnd bool) []kiroPendingBlock {
	start := len(blocks)
	message, ok := rawMessage.(map[string]any)
	if !ok {
		return blocks
	}
	role, _ := message["role"].(string)
	name, _ := message["name"].(string)

	if content, ok := message["content"]; ok {
		blocks = appendKiroChatCompletionsContentBlocks(ctx, alg, blocks, messageIndex, role, name, content)
	}
	if reasoning, ok := message["reasoning_content"].(string); ok && reasoning != "" {
		block := map[string]any{"type": "reasoning", "thinking": reasoning}
		blocks = append(blocks, kiroPendingBlock{
			value:  map[string]any{"kind": "message_reasoning", "message_index": messageIndex, "role": role, "name": name, "block": block},
			tokens: countKiroMessageContentTokens(ctx, alg, block),
		})
	}
	if toolCalls, ok := message["tool_calls"]; ok {
		value := kiroJSONCompatibleValue(toolCalls)
		blocks = append(blocks, kiroPendingBlock{
			value:  map[string]any{"kind": "message_tool_calls", "message_index": messageIndex, "role": role, "name": name, "block": value},
			tokens: countKiroSerializedValueTokens(alg, value),
		})
	}
	if toolCallID, ok := message["tool_call_id"].(string); ok && toolCallID != "" {
		value := map[string]any{"tool_call_id": toolCallID}
		blocks = append(blocks, kiroPendingBlock{
			value:  map[string]any{"kind": "message_tool_call_id", "message_index": messageIndex, "role": role, "name": name, "block": value},
			tokens: countKiroSerializedValueTokens(alg, value),
		})
	}
	if functionCall, ok := message["function_call"]; ok {
		value := kiroJSONCompatibleValue(functionCall)
		blocks = append(blocks, kiroPendingBlock{
			value:  map[string]any{"kind": "message_function_call", "message_index": messageIndex, "role": role, "name": name, "block": value},
			tokens: countKiroSerializedValueTokens(alg, value),
		})
	}
	return markKiroResponsesInputItemEnd(blocks, start, messageIndex, markMessageEnd)
}

func appendKiroChatCompletionsContentBlocks(ctx context.Context, alg anthropictokenizer.Algorithm, blocks []kiroPendingBlock, messageIndex int, role string, name string, content any) []kiroPendingBlock {
	switch typed := content.(type) {
	case string:
		block := map[string]any{"type": "text", "text": typed}
		return append(blocks, kiroPendingBlock{
			value:  map[string]any{"kind": "message", "message_index": messageIndex, "role": role, "name": name, "block_index": 0, "block": block},
			tokens: countKiroMessageContentTokens(ctx, alg, block),
		})
	case []any:
		for blockIndex, rawBlock := range typed {
			block := rawBlock
			if text, ok := rawBlock.(string); ok {
				block = map[string]any{"type": "text", "text": text}
			}
			blocks = append(blocks, kiroPendingBlock{
				value:  map[string]any{"kind": "message", "message_index": messageIndex, "role": role, "name": name, "block_index": blockIndex, "block": kiroJSONCompatibleValue(block)},
				tokens: countKiroMessageContentTokens(ctx, alg, block),
			})
		}
	case map[string]any:
		blocks = append(blocks, kiroPendingBlock{
			value:  map[string]any{"kind": "message", "message_index": messageIndex, "role": role, "name": name, "block_index": 0, "block": kiroJSONCompatibleValue(typed)},
			tokens: countKiroMessageContentTokens(ctx, alg, typed),
		})
	}
	return blocks
}

// applyKiroDefaultBreakpoints 在稳定历史消息末尾和 tools/system 前缀末尾放置断点。
//
// 最后一条消息属于本轮新输入，不能作为可复用前缀：若在它上面命中缓存，
// input_tokens 会变成 0，而真实上游本轮不可能已经缓存了当前输入。
//
// 注意它会无条件覆写 breakpointTTL。responses / chat_completions 是 OpenAI 形态、客户端
// 不下发 cache_control，无值可覆盖；Anthropic 路径必须先用 hasKiroClientCacheBreakpoint
// 判空后才可调用，否则会把客户端显式的 ttl:"1h" 降级成默认 5m。
func applyKiroDefaultBreakpoints(blocks []kiroPendingBlock, ttl time.Duration) {
	if len(blocks) == 0 {
		return
	}
	lastMessageIndex := -1
	for i := range blocks {
		if blocks[i].messageIndex != nil && *blocks[i].messageIndex > lastMessageIndex {
			lastMessageIndex = *blocks[i].messageIndex
		}
	}
	firstMessageBlock := len(blocks)
	for i := range blocks {
		if blocks[i].messageIndex != nil {
			firstMessageBlock = i
			break
		}
	}
	if firstMessageBlock > 0 {
		blocks[firstMessageBlock-1].breakpointTTL = &ttl
	}
	for i := range blocks {
		if blocks[i].isMessageEnd && blocks[i].messageIndex != nil && *blocks[i].messageIndex < lastMessageIndex {
			blocks[i].breakpointTTL = &ttl
		}
	}
}

// hasKiroClientCacheBreakpoint 判断客户端是否下发了任何 cache_control 断点。
func hasKiroClientCacheBreakpoint(blocks []kiroPendingBlock) bool {
	for i := range blocks {
		if blocks[i].breakpointTTL != nil {
			return true
		}
	}
	return false
}

func countKiroResponsesCacheableInputItems(input []any) int {
	count := 0
	for _, rawItem := range input {
		if isKiroResponsesCacheableInputItem(rawItem) {
			count++
		}
	}
	return count
}

func isKiroResponsesCacheableInputItem(rawItem any) bool {
	if _, ok := rawItem.(string); ok {
		return true
	}
	item, ok := rawItem.(map[string]any)
	if !ok {
		return false
	}
	itemType, _ := item["type"].(string)
	return itemType != "additional_tools"
}

func appendKiroResponsesInputItemBlocks(ctx context.Context, alg anthropictokenizer.Algorithm, blocks []kiroPendingBlock, itemIndex int, rawItem any, markMessageEnd bool) []kiroPendingBlock {
	start := len(blocks)
	if text, ok := rawItem.(string); ok {
		block := map[string]any{"type": "input_text", "text": text}
		blocks = append(blocks, kiroPendingBlock{
			value:  map[string]any{"kind": "input", "input_index": itemIndex, "role": "user", "block_index": 0, "block": block},
			tokens: countKiroMessageContentTokens(ctx, alg, block),
		})
		return markKiroResponsesInputItemEnd(blocks, start, itemIndex, markMessageEnd)
	}
	item, ok := rawItem.(map[string]any)
	if !ok {
		return blocks
	}
	itemType, _ := item["type"].(string)
	if itemType == "additional_tools" {
		return blocks
	}
	role, _ := item["role"].(string)
	if role == "" && (itemType == "message" || itemType == "") {
		role = "user"
	}
	if content, ok := item["content"]; ok {
		blocks = appendKiroResponsesContentBlocks(ctx, alg, blocks, itemIndex, role, content)
		return markKiroResponsesInputItemEnd(blocks, start, itemIndex, markMessageEnd)
	}
	value := kiroJSONCompatibleValue(item)
	blocks = append(blocks, kiroPendingBlock{
		value:  map[string]any{"kind": "input_item", "input_index": itemIndex, "type": itemType, "block": value},
		tokens: countKiroSerializedValueTokens(alg, value),
	})
	return markKiroResponsesInputItemEnd(blocks, start, itemIndex, markMessageEnd)
}

func markKiroResponsesInputItemEnd(blocks []kiroPendingBlock, start, itemIndex int, markMessageEnd bool) []kiroPendingBlock {
	if len(blocks) <= start {
		return blocks
	}
	last := len(blocks) - 1
	mi := itemIndex
	blocks[last].messageIndex = &mi
	blocks[last].isMessageEnd = markMessageEnd
	return blocks
}

func appendKiroResponsesContentBlocks(ctx context.Context, alg anthropictokenizer.Algorithm, blocks []kiroPendingBlock, itemIndex int, role string, content any) []kiroPendingBlock {
	switch typed := content.(type) {
	case string:
		block := map[string]any{"type": "input_text", "text": typed}
		return append(blocks, kiroPendingBlock{
			value:  map[string]any{"kind": "input", "input_index": itemIndex, "role": role, "block_index": 0, "block": block},
			tokens: countKiroMessageContentTokens(ctx, alg, block),
		})
	case []any:
		for blockIndex, rawBlock := range typed {
			block := rawBlock
			if text, ok := rawBlock.(string); ok {
				block = map[string]any{"type": "input_text", "text": text}
			}
			blocks = append(blocks, kiroPendingBlock{
				value:  map[string]any{"kind": "input", "input_index": itemIndex, "role": role, "block_index": blockIndex, "block": kiroJSONCompatibleValue(block)},
				tokens: countKiroMessageContentTokens(ctx, alg, block),
			})
		}
	case map[string]any:
		blocks = append(blocks, kiroPendingBlock{
			value:  map[string]any{"kind": "input", "input_index": itemIndex, "role": role, "block_index": 0, "block": kiroJSONCompatibleValue(typed)},
			tokens: countKiroMessageContentTokens(ctx, alg, typed),
		})
	}
	return blocks
}

func countKiroResponsesInputTokens(payload map[string]any, blocks []kiroPendingBlock, toolCount int) int {
	tokens := toolCount * kiroTokensPerTool
	for _, block := range blocks {
		tokens += max(block.tokens, 0)
	}
	if input, ok := payload["input"].([]any); ok {
		tokens += len(input) * kiroTokensPerMessage
	}
	return max(tokens, 1)
}

func countKiroChatCompletionsInputTokens(payload map[string]any, blocks []kiroPendingBlock, toolCount int) int {
	tokens := toolCount * kiroTokensPerTool
	for _, block := range blocks {
		tokens += max(block.tokens, 0)
	}
	if messages, ok := payload["messages"].([]any); ok {
		tokens += len(messages) * kiroTokensPerMessage
	}
	return max(tokens, 1)
}

func kiroNestedValue(payload map[string]any, keys ...string) any {
	var current any = payload
	for _, key := range keys {
		m, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = m[key]
	}
	return current
}

func kiroJSONCompatibleValue(value any) any {
	b, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return value
	}
	return out
}

func normalizeKiroSystemBlocks(system any) []any {
	switch typed := system.(type) {
	case nil:
		return nil
	case string:
		return []any{map[string]any{"type": "text", "text": typed}}
	case []any:
		return typed
	default:
		return []any{typed}
	}
}

func canonicalizeKiroSystemBlock(value any) {
	obj, ok := value.(map[string]any)
	if !ok {
		return
	}
	blockType, _ := obj["type"].(string)
	if blockType != "" && blockType != "text" {
		return
	}
	text, _ := obj["text"].(string)
	if strings.HasPrefix(text, "x-anthropic-billing-header:") {
		obj["text"] = "__anthropic_billing_header__"
	}
}

func extractKiroCacheTTL(value any) *time.Duration {
	obj, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	cc, ok := obj["cache_control"].(map[string]any)
	if !ok || !strings.EqualFold(strings.TrimSpace(kiroCacheAsString(cc["type"])), "ephemeral") {
		return nil
	}
	ttl := kiroCacheDefaultTTL
	if strings.EqualFold(strings.TrimSpace(kiroCacheAsString(cc["ttl"])), "1h") {
		ttl = kiroCacheOneHourTTL
	}
	return &ttl
}

func (p *kiroCacheProfile) resolvedBreakpoints() []kiroResolvedBreakpoint {
	if p == nil {
		return nil
	}
	resolved := make([]kiroResolvedBreakpoint, 0, len(p.breakpoints))
	for _, breakpoint := range p.breakpoints {
		if breakpoint.blockIndex < 0 || breakpoint.blockIndex >= len(p.blocks) {
			continue
		}
		block := p.blocks[breakpoint.blockIndex]
		if block.cumulativeTokens <= 0 {
			continue
		}
		resolved = append(resolved, kiroResolvedBreakpoint{blockIndex: breakpoint.blockIndex, cumulativeTokens: block.cumulativeTokens, ttl: breakpoint.ttl})
	}
	return resolved
}

func (p *kiroCacheProfile) lastBreakpoint() *kiroResolvedBreakpoint {
	breakpoints := p.resolvedBreakpoints()
	if len(breakpoints) == 0 {
		return nil
	}
	last := breakpoints[len(breakpoints)-1]
	return &last
}

func (t *kiroCacheTracker) compute(cacheKey uint64, profile *kiroCacheProfile, inputTokens int, creationRatio, readRatio float64) *kiroCacheEmulationUsage {
	if t == nil || profile == nil || cacheKey == 0 {
		return nil
	}
	lastBreakpoint := profile.lastBreakpoint()
	if lastBreakpoint == nil {
		return nil
	}
	total := inputTokens
	if total <= 0 {
		total = profile.totalInputTokens
	}
	total = max(total, 0)
	coveredEst := max(lastBreakpoint.cumulativeTokens, 0)
	if coveredEst <= 0 || profile.profileTotalEst <= 0 || total <= 0 {
		return nil
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked(cacheKey, now)

	matchedTokens := 0
	if namespaceEntries := t.entries[cacheKey]; namespaceEntries != nil {
		breakpoints := profile.resolvedBreakpoints()
		for i, seen := len(breakpoints)-1, 0; i >= 0 && seen < kiroCachePrefixLookbackLimit; i, seen = i-1, seen+1 {
			breakpoint := breakpoints[i]
			candidate := profile.blocks[breakpoint.blockIndex]
			entry, ok := namespaceEntries[candidate.prefixFingerprint]
			if !ok || !entry.expiresAt.After(now) {
				continue
			}
			// 只续期命中的这一条即可：整条前缀链的续期由 commit() → update() 完成，
			// 它会遍历当前 profile 的全部 cacheableBreakpoints 并推后 expiresAt，而命中点
			// 之前的断点都属于当前 profile。此处再遍历一遍是冗余的。
			entry.expiresAt = now.Add(entry.ttl)
			entry.lastHitAt = now
			namespaceEntries[candidate.prefixFingerprint] = entry
			matchedTokens = breakpoint.cumulativeTokens
			break
		}
	}

	coveredRatio := float64(coveredEst) / float64(profile.profileTotalEst)
	coveredRatio = math.Min(math.Max(coveredRatio, 0), 1)
	cacheTotal := min(int(math.Round(float64(total)*coveredRatio)), total)
	if cacheTotal < profile.minCacheable {
		return nil
	}
	read := min(max(int(math.Round(float64(cacheTotal)*float64(matchedTokens)/float64(coveredEst))), 0), cacheTotal)
	creation := cacheTotal - read
	read = scaleKiroCacheTokens(read, readRatio)
	creation = scaleKiroCacheTokens(creation, creationRatio)
	creation5mEst, creation1hEst := profile.creationTTLEstimates(matchedTokens)
	creation5m, creation1h := splitKiroCreationByTTL(creation, creation5mEst, creation1hEst)
	return &kiroCacheEmulationUsage{
		InputTokens:                total - read - creation,
		CacheReadInputTokens:       read,
		CacheCreationInputTokens:   creation,
		CacheCreation5mInputTokens: creation5m,
		CacheCreation1hInputTokens: creation1h,
	}
}

func (p *kiroCacheProfile) creationTTLEstimates(readTokens int) (int, int) {
	if p == nil {
		return 0, 0
	}
	est5m, est1h := 0, 0
	previous := max(readTokens, 0)
	for _, breakpoint := range p.resolvedBreakpoints() {
		if breakpoint.cumulativeTokens <= previous {
			continue
		}
		delta := breakpoint.cumulativeTokens - previous
		if breakpoint.ttl >= kiroCacheOneHourTTL {
			est1h += delta
		} else {
			est5m += delta
		}
		previous = breakpoint.cumulativeTokens
	}
	return est5m, est1h
}

func splitKiroCreationByTTL(creation, est5m, est1h int) (int, int) {
	if creation <= 0 {
		return 0, 0
	}
	totalEst := est5m + est1h
	if totalEst <= 0 {
		return creation, 0
	}
	creation5m := min(max(int(math.Round(float64(creation)*float64(est5m)/float64(totalEst))), 0), creation)
	return creation5m, creation - creation5m
}

func (t *kiroCacheTracker) update(cacheKey uint64, profile *kiroCacheProfile) {
	if t == nil || profile == nil || cacheKey == 0 {
		return
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked(cacheKey, now)
	namespaceEntries := t.entries[cacheKey]
	if namespaceEntries == nil {
		namespaceEntries = make(map[[32]byte]kiroCacheEntry)
		t.entries[cacheKey] = namespaceEntries
	}
	for _, breakpoint := range profile.resolvedBreakpoints() {
		block := profile.blocks[breakpoint.blockIndex]
		expiresAt := now.Add(breakpoint.ttl)
		entry, ok := namespaceEntries[block.prefixFingerprint]
		if ok {
			entry.tokens = max(entry.tokens, block.cumulativeTokens)
			entry.ttl = maxDuration(entry.ttl, breakpoint.ttl)
			if expiresAt.After(entry.expiresAt) {
				entry.expiresAt = expiresAt
			}
			entry.lastHitAt = now
			namespaceEntries[block.prefixFingerprint] = entry
			continue
		}
		namespaceEntries[block.prefixFingerprint] = kiroCacheEntry{
			tokens: block.cumulativeTokens, ttl: breakpoint.ttl, expiresAt: expiresAt, lastHitAt: now,
		}
		t.entryCount++
	}
	t.enforceCapacityLocked(cacheKey)
}

func (t *kiroCacheTracker) pruneLocked(touchedKey uint64, now time.Time) {
	t.pruneNamespaceLocked(touchedKey, now)
	if !t.lastFullPrune.IsZero() && now.Sub(t.lastFullPrune) < kiroCacheFullPruneInterval {
		return
	}
	for cacheKey := range t.entries {
		if cacheKey != touchedKey {
			t.pruneNamespaceLocked(cacheKey, now)
		}
	}
	t.lastFullPrune = now
}

func (t *kiroCacheTracker) pruneNamespaceLocked(cacheKey uint64, now time.Time) {
	namespaceEntries := t.entries[cacheKey]
	for fp, entry := range namespaceEntries {
		if !entry.expiresAt.After(now) {
			delete(namespaceEntries, fp)
			t.entryCount--
		}
	}
	if len(namespaceEntries) == 0 {
		delete(t.entries, cacheKey)
	}
}

func (t *kiroCacheTracker) enforceCapacityLocked(touchedKey uint64) {
	namespaceCapacity := t.capacity
	if namespaceCapacity <= 0 {
		namespaceCapacity = kiroCacheNamespaceCapacity
	}
	t.evictLeastRecentlyUsedLocked(touchedKey, len(t.entries[touchedKey])-namespaceCapacity)

	globalCapacity := t.globalCapacity
	if globalCapacity <= 0 {
		globalCapacity = kiroCacheGlobalCapacity
	}
	if t.entryCount <= globalCapacity {
		return
	}
	t.evictLeastRecentlyUsedLocked(0, t.entryCount-globalCapacity)
}

func (t *kiroCacheTracker) evictLeastRecentlyUsedLocked(namespace uint64, count int) {
	if count <= 0 {
		return
	}
	type victim struct {
		namespace   uint64
		fingerprint [32]byte
		lastHitAt   time.Time
	}
	victims := make([]victim, 0, count)
	for cacheKey, namespaceEntries := range t.entries {
		if namespace != 0 && cacheKey != namespace {
			continue
		}
		for fp, entry := range namespaceEntries {
			if len(victims) < count {
				victims = append(victims, victim{namespace: cacheKey, fingerprint: fp, lastHitAt: entry.lastHitAt})
				sort.Slice(victims, func(i, j int) bool {
					return victims[i].lastHitAt.Before(victims[j].lastHitAt)
				})
				continue
			}
			if !entry.lastHitAt.Before(victims[len(victims)-1].lastHitAt) {
				continue
			}
			victims[len(victims)-1] = victim{namespace: cacheKey, fingerprint: fp, lastHitAt: entry.lastHitAt}
			sort.Slice(victims, func(i, j int) bool {
				return victims[i].lastHitAt.Before(victims[j].lastHitAt)
			})
		}
	}
	for _, victim := range victims {
		entries := t.entries[victim.namespace]
		if _, ok := entries[victim.fingerprint]; !ok {
			continue
		}
		delete(entries, victim.fingerprint)
		t.entryCount--
		if len(entries) == 0 {
			delete(t.entries, victim.namespace)
		}
	}
}

// kiroCacheCredentialKey returns the tracker namespace for a client session.
//
// The upstream account is only an egress choice and is not the cache identity.
// Prefer the Claude Code metadata session, then the client API key. Falling back
// to account.ID preserves the previous behavior when neither client identity is
// available. Including the model in this fingerprint prelude prevents the same
// session from reusing one model's cache accounting for another model.
//
// Never use rotating OAuth credentials here: refresh_token and access_token are
// overwritten during normal refreshes, which would invalidate a live session.
func kiroCacheCredentialKey(account *Account, parsed *ParsedRequest, model string) uint64 {
	if account == nil || account.ID <= 0 {
		return 0
	}
	h := fnv.New64a()
	isolation := kiroPromptCacheIsolationForParsed(parsed)
	switch {
	case isolation.SessionID != "":
		_, _ = h.Write([]byte("session:" + isolation.SessionID + "\x00"))
	case isolation.APIKeyID > 0:
		_, _ = h.Write([]byte("api_key:" + fmt.Sprint(isolation.APIKeyID) + "\x00"))
	default:
		_, _ = h.Write([]byte("account:" + fmt.Sprint(account.ID) + "\x00"))
	}
	_, _ = h.Write([]byte("model:" + strings.TrimSpace(model)))
	return h.Sum64()
}

func kiroCacheCredentialIdentity(account *Account) string {
	if account == nil {
		return ""
	}
	parts := make([]string, 0, 8)
	for _, key := range []string{"client_id_hash", "client_id", "refresh_token", "profile_arn", "kiro_api_key", "kiroApiKey", "api_key"} {
		if value := strings.TrimSpace(account.GetCredential(key)); value != "" {
			parts = append(parts, key+":"+value)
		}
	}
	if len(parts) == 0 && account.ID > 0 {
		parts = append(parts, "account:"+fmt.Sprint(account.ID))
	}
	return strings.Join(parts, "|")
}

// kiroMinimumCacheableTokens 返回「前缀至少多少 token 才值得记进缓存」的阈值。
// 目前只有两档特例：GPT-5.6 系列取 1024（对齐 OpenAI 官方最小缓存粒度），opus 系取
// 4096；其余 Kiro 模型走默认档。GPT 用 kiropkg.IsKiroGPTModel 精确匹配，opus 仍用
// 子串以覆盖 -thinking 与带日期后缀的变体。
// 各模型的期望值由 TestKiroMinimumCacheableTokens 钉死。
func kiroMinimumCacheableTokens(model string) int {
	if kiropkg.IsKiroGPTModel(model) {
		return kiroCacheMinTokensGPT
	}
	if strings.Contains(strings.ToLower(model), "opus") {
		return kiroCacheMinTokensOpus
	}
	return kiroCacheMinTokensDefault
}

func stripKiroCacheControl(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, child := range x {
			if k == "cache_control" {
				continue
			}
			out[k] = stripKiroCacheControl(child)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, child := range x {
			out[i] = stripKiroCacheControl(child)
		}
		return out
	default:
		return v
	}
}

func countKiroInputTokensFromPayload(ctx context.Context, alg anthropictokenizer.Algorithm, payload map[string]any) int {
	if payload == nil {
		return 1
	}
	tokens := 0
	for _, block := range normalizeKiroSystemBlocks(payload["system"]) {
		tokens += countKiroSystemBlockTokens(alg, block)
	}
	messages, _ := payload["messages"].([]any)
	if len(messages) > 0 {
		sanitizedMessages, imageTokens := sanitizeKiroImagesForTokenEstimate(ctx, messages)
		canonical, err := canonicalJSON(sanitizedMessages)
		if err == nil {
			tokens += alg.CountTokens(string(canonical))
		}
		tokens += imageTokens
		tokens += len(messages) * kiroTokensPerMessage
	}
	if tools, ok := payload["tools"].([]any); ok {
		tokens += len(tools) * kiroTokensPerTool
	}
	return max(tokens, 1)
}

func countKiroSystemBlockTokens(alg anthropictokenizer.Algorithm, value any) int {
	switch typed := value.(type) {
	case string:
		return alg.CountTokens(typed)
	case map[string]any:
		if text, ok := typed["text"].(string); ok {
			return alg.CountTokens(text)
		}
		return 0
	default:
		return 0
	}
}

func countKiroMessageContentTokens(ctx context.Context, alg anthropictokenizer.Algorithm, value any) int {
	switch typed := value.(type) {
	case nil:
		return 0
	case string:
		return alg.CountTokens(typed)
	case []any:
		total := 0
		for _, item := range typed {
			total += countKiroMessageContentTokens(ctx, alg, item)
		}
		return total
	case map[string]any:
		if mediaType, source, ok := kiroImageTokenSource(typed); ok {
			return kiropkg.EstimateImageTokens(ctx, mediaType, source)
		}
		if text, ok := typed["text"].(string); ok {
			return alg.CountTokens(text)
		}
		if thinking, ok := typed["thinking"].(string); ok {
			return alg.CountTokens(thinking)
		}
		if input, ok := typed["input"]; ok {
			return countKiroSerializedValueTokens(alg, input)
		}
		if content, ok := typed["content"]; ok {
			return countKiroMessageContentTokens(ctx, alg, content)
		}
		return 0
	default:
		return 0
	}
}

func sanitizeKiroImagesForTokenEstimate(ctx context.Context, value any) (any, int) {
	switch typed := value.(type) {
	case []any:
		out := make([]any, len(typed))
		tokens := 0
		for i, item := range typed {
			out[i], tokens = sanitizeKiroImageItem(ctx, item, tokens)
		}
		return out, tokens
	case map[string]any:
		if mediaType, source, ok := kiroImageTokenSource(typed); ok {
			return sanitizeKiroImageBlock(typed), kiropkg.EstimateImageTokens(ctx, mediaType, source)
		}
		out := make(map[string]any, len(typed))
		tokens := 0
		for key, item := range typed {
			out[key], tokens = sanitizeKiroImageItem(ctx, item, tokens)
		}
		return out, tokens
	default:
		return value, 0
	}
}

func sanitizeKiroImageItem(ctx context.Context, value any, currentTokens int) (any, int) {
	sanitized, tokens := sanitizeKiroImagesForTokenEstimate(ctx, value)
	return sanitized, currentTokens + tokens
}

func sanitizeKiroImageBlock(value map[string]any) map[string]any {
	out := make(map[string]any, len(value))
	for key, item := range value {
		switch typed := item.(type) {
		case map[string]any:
			out[key] = sanitizeKiroImageBlock(typed)
		case []any:
			items := make([]any, len(typed))
			for i, child := range typed {
				if childMap, ok := child.(map[string]any); ok {
					items[i] = sanitizeKiroImageBlock(childMap)
				} else {
					items[i] = child
				}
			}
			out[key] = items
		case string:
			lowerKey := strings.ToLower(key)
			if lowerKey == "data" || lowerKey == "url" || lowerKey == "image_url" {
				out[key] = "[image]"
			} else {
				out[key] = typed
			}
		default:
			out[key] = item
		}
	}
	return out
}

func kiroImageTokenSource(value map[string]any) (mediaType, source string, ok bool) {
	kind, _ := value["type"].(string)
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "image":
		mediaType, source = kiroImageSourceFields(value)
		return mediaType, source, true
	case "image_url", "input_image":
		mediaType, source = kiroImageSourceFields(value)
		if raw, exists := value["image_url"]; exists {
			switch typed := raw.(type) {
			case string:
				source = typed
			case map[string]any:
				if url, found := typed["url"].(string); found {
					source = url
				}
			}
		}
		return mediaType, source, true
	default:
		return "", "", false
	}
}

func kiroImageSourceFields(value map[string]any) (mediaType, source string) {
	container := value
	if nested, ok := value["source"].(map[string]any); ok {
		container = nested
	}
	for _, key := range []string{"media_type", "mediaType", "mime_type"} {
		if candidate, ok := container[key].(string); ok && strings.TrimSpace(candidate) != "" {
			mediaType = candidate
			break
		}
	}
	for _, key := range []string{"data", "url"} {
		if candidate, ok := container[key].(string); ok && strings.TrimSpace(candidate) != "" {
			source = candidate
			break
		}
	}
	return mediaType, source
}

func countKiroSerializedValueTokens(alg anthropictokenizer.Algorithm, value any) int {
	canonical, err := canonicalJSON(value)
	if err != nil {
		return 0
	}
	return alg.CountTokens(string(canonical))
}

func canonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeCanonicalJSON(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonicalJSON(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		_ = buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				_ = buf.WriteByte(',')
			}
			kb, _ := json.Marshal(k)
			_, _ = buf.Write(kb)
			_ = buf.WriteByte(':')
			if err := writeCanonicalJSON(buf, x[k]); err != nil {
				return err
			}
		}
		_ = buf.WriteByte('}')
		return nil
	case []any:
		_ = buf.WriteByte('[')
		for i, child := range x {
			if i > 0 {
				_ = buf.WriteByte(',')
			}
			if err := writeCanonicalJSON(buf, child); err != nil {
				return err
			}
		}
		_ = buf.WriteByte(']')
		return nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		_, _ = buf.Write(b)
		return nil
	}
}

func kiroCacheAsString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func (u *kiroCacheEmulationUsage) toKiroUsage() *kiropkg.Usage {
	if u == nil {
		return nil
	}
	return &kiropkg.Usage{
		InputTokens:                u.InputTokens,
		CacheReadInputTokens:       u.CacheReadInputTokens,
		CacheCreationInputTokens:   u.CacheCreationInputTokens,
		CacheCreation5mInputTokens: u.CacheCreation5mInputTokens,
		CacheCreation1hInputTokens: u.CacheCreation1hInputTokens,
	}
}
