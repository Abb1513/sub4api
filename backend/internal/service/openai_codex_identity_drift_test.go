package service

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const (
	testV7AnchorMs     int64 = codexStableUUIDv7BaseMs - 3600_000
	testV7NearAnchorMs int64 = codexStableUUIDv7BaseMs - 1000
)

// v7 时间戳必须恒在过去：官方 Uuid::now_v7() 永不产生未来时间戳，
// 未来的时间戳与 turn_started_at / 请求到达时间交叉验证即暴露合成身份。
func TestDeriveCodexStableTimestampMsAlwaysPast(t *testing.T) {
	now := time.Now().UnixMilli()
	for _, seed := range []string{"", "a", "b", "session:11", "session:19", "thread:xyz"} {
		got := deriveCodexStableTimestampMs(seed)
		require.Less(t, got, int64(codexStableUUIDv7BaseMs), "seed %q 必须早于锚点", seed)
		require.GreaterOrEqual(t, got, int64(codexStableUUIDv7BaseMs)-int64(codexStableUUIDv7SpanMs), "seed %q 仍在 200 天窗口内", seed)
		require.Less(t, got, now, "seed %q 不得落在未来", seed)
		require.Equal(t, got, deriveCodexStableTimestampMs(seed), "跨调用必须确定")
	}
	// 种子必须真的散列开，否则所有会话共享一个创建时刻。
	require.NotEqual(t, deriveCodexStableTimestampMs("a"), deriveCodexStableTimestampMs("b"))
}

// v7 的时间位必须精确等于 anchor：官方 turn_id 与同一请求的
// turn_started_at_unix_ms 由同一时刻生成。
func TestDeriveStableCodexUUIDv7TimestampEqualsAnchor(t *testing.T) {
	id := deriveStableCodexUUIDv7("seed-x", testV7AnchorMs)
	parsed, err := uuid.Parse(id)
	require.NoError(t, err)
	require.Equal(t, uuid.Version(7), parsed.Version())
	got, ok := codexV7TimestampMs(id)
	require.True(t, ok)
	require.Equal(t, testV7AnchorMs, got)
	// 唯一性来自哈希随机字段，而非时间戳散列。
	require.NotEqual(t, id, deriveStableCodexUUIDv7("seed-y", testV7AnchorMs))
}

func TestCodexV7TimestampMsRejectsNonV7(t *testing.T) {
	v4 := deriveStableUUIDv4("installation-seed")
	_, ok := codexV7TimestampMs(v4)
	require.False(t, ok, "v4 不是 v7，必须回退到派生锚点")
	_, ok = codexV7TimestampMs("not-a-uuid")
	require.False(t, ok)
	_, ok = codexV7TimestampMs(deriveStableCodexUUIDv7("s", testV7NearAnchorMs))
	require.True(t, ok)
}

// 账号级身份改写必须保持官方版本语义：
// session/thread/turn 为 Uuid::now_v7()，installation 为 Uuid::new_v4()。
func TestScopeCodexAccountIdentityKeepsOfficialUUIDVersions(t *testing.T) {
	account := &Account{ID: 11, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "chatgpt-account-11"}}
	const apiKeyID = 77

	v7Raw := deriveStableCodexUUIDv7("client-session", testV7AnchorMs)
	for _, kind := range []string{"session", "thread", "turn"} {
		scoped := scopeCodexAccountIdentityValue(account, apiKeyID, kind, v7Raw)
		parsed, err := uuid.Parse(scoped)
		require.NoError(t, err, kind)
		require.Equal(t, uuid.Version(7), parsed.Version(), kind)
		// 改写后仍保留原值的创建时刻：turn_id ≈ turn_started_at 的一致性不被破坏。
		want, ok := codexV7TimestampMs(v7Raw)
		require.True(t, ok)
		got, ok := codexV7TimestampMs(scoped)
		require.True(t, ok)
		require.Equal(t, want, got, kind)
	}

	scopedInstall := scopeCodexAccountIdentityValue(account, apiKeyID, "installation", "some-installation")
	installParsed, err := uuid.Parse(scopedInstall)
	require.NoError(t, err)
	require.Equal(t, uuid.Version(4), installParsed.Version(), "installation_id 官方为 new_v4")

	// 非 v7 原值退回稳定过去锚点，仍是合法 v7。
	scopedLegacy := scopeCodexAccountIdentityValue(account, apiKeyID, "session", "legacy-session-raw")
	legacyParsed, err := uuid.Parse(scopedLegacy)
	require.NoError(t, err)
	require.Equal(t, uuid.Version(7), legacyParsed.Version())
	legacyTs, ok := codexV7TimestampMs(scopedLegacy)
	require.True(t, ok)
	require.Less(t, legacyTs, time.Now().UnixMilli())

	// window 保留 thread 身份 + 序号：thread 部分换成 v7，序号原样。
	scopedWindow := scopeCodexAccountIdentityValue(account, apiKeyID, "window", v7Raw+":3")
	threadPart, ordinal, ok := splitCodexWindowID(scopedWindow)
	require.True(t, ok)
	require.Equal(t, "3", ordinal)
	require.Equal(t, scopeCodexAccountIdentityValue(account, apiKeyID, "thread", v7Raw), threadPart)
}

// 同账号下不同会话不得共用 session_id（防重复），同会话跨轮必须恒定（防抖动），
// 跨账号即便内容相同也不得共享（会话+账号关联）。
func TestDeriveCodexFallbackSessionIDDistinguishesConversations(t *testing.T) {
	account11 := &Account{ID: 11, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "chatgpt-account-11"}}
	account19 := &Account{ID: 19, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "chatgpt-account-19"}}

	bodyA := []byte(`{"model":"gpt-5-codex","instructions":"sys","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"第一号话题"}]}]}`)
	bodyB := []byte(`{"model":"gpt-5-codex","instructions":"sys","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"第二号话题"}]}]}`)

	sameA := deriveCodexFallbackSessionID(account11, bodyA)
	require.Equal(t, sameA, deriveCodexFallbackSessionID(account11, bodyA), "同一会话跨轮必须恒定")
	require.NotEqual(t, sameA, deriveCodexFallbackSessionID(account11, bodyB), "同账号不同会话必须区分（旧实现在此塌缩）")
	require.NotEqual(t, sameA, deriveCodexFallbackSessionID(account19, bodyA), "不同账号不得共享 session_id")

	parsed, err := uuid.Parse(sameA)
	require.NoError(t, err)
	require.Equal(t, uuid.Version(7), parsed.Version())
	ts, ok := codexV7TimestampMs(sameA)
	require.True(t, ok)
	require.Less(t, ts, time.Now().UnixMilli())

	// 响应链（主键）区分不同对话。
	chainA := []byte(`{"model":"gpt-5-codex","previous_response_id":"resp_alpha"}`)
	chainB := []byte(`{"model":"gpt-5-codex","previous_response_id":"resp_beta"}`)
	require.Equal(t, deriveCodexFallbackSessionID(account11, chainA), deriveCodexFallbackSessionID(account11, chainA))
	require.NotEqual(t, deriveCodexFallbackSessionID(account11, chainA), deriveCodexFallbackSessionID(account11, chainB))

	// 优先级与内容审计一致：响应链先，内容哈希兜底。
	// 同内容不同 resp → 必须按 resp 区分（resp 优先，不被内容掩盖）。
	contentWithRespA := []byte(`{"model":"gpt-5-codex","instructions":"sys","previous_response_id":"resp_alpha","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"第一号话题"}]}]}`)
	contentWithRespB := []byte(`{"model":"gpt-5-codex","instructions":"sys","previous_response_id":"resp_beta","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"第一号话题"}]}]}`)
	require.NotEqual(t, deriveCodexFallbackSessionID(account11, contentWithRespA), deriveCodexFallbackSessionID(account11, contentWithRespB))
	// 同 resp 不同内容 → 必须仍是同一 session（resp 优先，内容不得改写它）。
	sameRespOtherContent := []byte(`{"model":"gpt-5-codex","instructions":"sys","previous_response_id":"resp_alpha","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"另一个话题"}]}]}`)
	require.Equal(t, deriveCodexFallbackSessionID(account11, contentWithRespA), deriveCodexFallbackSessionID(account11, sameRespOtherContent))
	// 不得把 model+tools 当租户级会话身份。
	modelOnly := []byte(`{"model":"gpt-5-codex","tools":[{"type":"function"}]}`)
	require.Equal(t, deriveCodexFallbackSessionID(account11, modelOnly), deriveCodexFallbackSessionID(account11, modelOnly))
	require.NotEqual(t, deriveCodexFallbackSessionID(account11, modelOnly), deriveCodexFallbackSessionID(account19, modelOnly))

	// 伪造的 previous_response_id 不得成为关联键（只认真实响应链）。
	bogus := []byte(`{"model":"gpt-5-codex","previous_response_id":"client-side-garbage"}`)
	require.Equal(t, deriveCodexFallbackSessionID(account11, bogus), deriveCodexFallbackSessionID(account11, modelOnly))
}

// compact 探测与真实转发打同一端点，探测 session 也必须是官方形态的 v7。
func TestCompactProbeSessionIDIsOfficialShaped(t *testing.T) {
	for _, accountID := range []int64{0, 11, 19} {
		id := compactProbeSessionID(accountID)
		parsed, err := uuid.Parse(id)
		require.NoError(t, err)
		require.Equal(t, uuid.Version(7), parsed.Version())
		ts, ok := codexV7TimestampMs(id)
		require.True(t, ok)
		require.Less(t, ts, time.Now().UnixMilli())
	}
	require.Equal(t, compactProbeSessionID(11), compactProbeSessionID(11), "重复探测复用同一会话")
	require.NotEqual(t, compactProbeSessionID(11), compactProbeSessionID(19))
}
