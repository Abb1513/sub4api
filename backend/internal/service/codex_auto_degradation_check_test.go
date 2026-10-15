package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/pkg/xai"
	"github.com/stretchr/testify/require"
)

// guardRuntimeModelMappingOptions 保护 parseSettings 发布的进程级 Grok
// model_mapping 运行时选项：parseSettings 会调用 xai.SetRuntimeModelMappingOptions
// （见 setting_parse.go），不还原会把 cross-client 默认映射泄漏给同包后续测试
// （如 openai_model_mapping_test.go 的 Grok 别名断言）。与
// account_wildcard_test.go 的做法一致。
func guardRuntimeModelMappingOptions(t *testing.T) {
	t.Helper()
	original := xai.RuntimeModelMappingOptions()
	t.Cleanup(func() { xai.SetRuntimeModelMappingOptions(original) })
}

// ---------------------------------------------------------------------------
// stubs
// ---------------------------------------------------------------------------

type codexDegradationSetCall struct {
	accountID int64
	until     time.Time
	reason    string
}

// codexDegradationRepoStub 模拟 accounts 表：SetTempUnschedulable 保持与
// 生产 SQL 一致的“单调只延长”语义，便于断言降级续期行为。
type codexDegradationRepoStub struct {
	AccountRepository

	mu            sync.Mutex
	accounts      map[int64]*Account
	setCalls      []codexDegradationSetCall
	clearCalls    []int64
	extraUpdates  []int64
	dueAccounts   []Account
	listCallCount int
}

func newCodexDegradationRepoStub(accounts ...*Account) *codexDegradationRepoStub {
	repo := &codexDegradationRepoStub{accounts: map[int64]*Account{}}
	for _, account := range accounts {
		repo.accounts[account.ID] = account
	}
	return repo
}

func (r *codexDegradationRepoStub) GetByID(_ context.Context, id int64) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	account := r.accounts[id]
	if account == nil {
		return nil, ErrAccountNotFound
	}
	clone := *account
	clone.Extra = mergeMap(nil, account.Extra)
	return &clone, nil
}

func (r *codexDegradationRepoStub) SetTempUnschedulable(_ context.Context, id int64, until time.Time, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.setCalls = append(r.setCalls, codexDegradationSetCall{accountID: id, until: until, reason: reason})
	account := r.accounts[id]
	if account == nil {
		return ErrAccountNotFound
	}
	// 单调只延长：与生产 SQL 的 WHERE temp_unschedulable_until IS NULL OR < $1 一致。
	if account.TempUnschedulableUntil == nil || account.TempUnschedulableUntil.Before(until) {
		account.TempUnschedulableUntil = &until
		account.TempUnschedulableReason = reason
	}
	return nil
}

func (r *codexDegradationRepoStub) ClearTempUnschedulable(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clearCalls = append(r.clearCalls, id)
	account := r.accounts[id]
	if account == nil {
		return ErrAccountNotFound
	}
	account.TempUnschedulableUntil = nil
	account.TempUnschedulableReason = ""
	return nil
}

func (r *codexDegradationRepoStub) ClearModelRateLimits(_ context.Context, _ int64) error {
	return nil
}

func (r *codexDegradationRepoStub) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	account := r.accounts[id]
	if account == nil {
		return ErrAccountNotFound
	}
	if account.Extra == nil {
		account.Extra = make(map[string]any)
	}
	for key, value := range updates {
		account.Extra[key] = value
	}
	r.extraUpdates = append(r.extraUpdates, id)
	return nil
}

func (r *codexDegradationRepoStub) ListDueCodexAutoDegradationCheckAccounts(_ context.Context, _ time.Time, limit int) ([]Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listCallCount++
	result := make([]Account, 0, len(r.dueAccounts))
	for _, account := range r.dueAccounts {
		if len(result) >= limit {
			break
		}
		result = append(result, account)
	}
	return result, nil
}

func (r *codexDegradationRepoStub) storedState(t *testing.T, id int64) *CodexAutoDegradationCheckState {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	account := r.accounts[id]
	require.NotNil(t, account)
	state := decodeCodexAutoDegradationCheckState(account.Extra)
	require.NotNil(t, state, "expected persisted check state")
	return state
}

func (r *codexDegradationRepoStub) hasStoredState(t *testing.T, id int64) bool {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	account := r.accounts[id]
	require.NotNil(t, account)
	return decodeCodexAutoDegradationCheckState(account.Extra) != nil
}

type codexDegradationProbeCall struct {
	accountID int64
	model     string
}

type codexDegradationProbeStub struct {
	mu    sync.Mutex
	calls []codexDegradationProbeCall
	next  func(accountID int64, model string) (CodexDiagnosticVerdict, error)
}

func (p *codexDegradationProbeStub) ProbeCodexAutoDegradation(_ context.Context, accountID int64, model string) (CodexDiagnosticVerdict, error) {
	p.mu.Lock()
	p.calls = append(p.calls, codexDegradationProbeCall{accountID: accountID, model: model})
	handler := p.next
	p.mu.Unlock()
	return handler(accountID, model)
}

func (p *codexDegradationProbeStub) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func (p *codexDegradationProbeStub) probedIDs() []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := make([]int64, 0, len(p.calls))
	for _, call := range p.calls {
		ids = append(ids, call.accountID)
	}
	return ids
}

func fixedVerdict(status string) *codexDegradationProbeStub {
	return &codexDegradationProbeStub{
		next: func(_ int64, _ string) (CodexDiagnosticVerdict, error) {
			return CodexDiagnosticVerdict{Status: status}, nil
		},
	}
}

func newCodexDegradationTestService(
	repo AccountRepository,
	probe codexAutoDegradationProbe,
	settings map[string]string,
) *CodexAutoDegradationCheckService {
	settingRepo := &upstreamBillingProbeSettingRepo{values: settings}
	settingService := NewSettingService(settingRepo, &config.Config{})
	rateLimitService := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	return NewCodexAutoDegradationCheckService(repo, settingService, rateLimitService, probe)
}

func enabledDegradationSettings() map[string]string {
	return map[string]string{
		SettingKeyCodexAutoDegradationCheck: `{"enabled":true,"interval_minutes":10,"block_minutes":30,"models":["gpt-6.1-sol"]}`,
	}
}

func activeOpenAIOAuthAccount(id int64) *Account {
	return &Account{
		ID:       id,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Status:   StatusActive,
	}
}

var errProbeBoom = errors.New("boom")

// ---------------------------------------------------------------------------
// settings: defaults / parse / validate / round-trip
// ---------------------------------------------------------------------------

func TestCodexAutoDegradationCheckSettingsDefaults(t *testing.T) {
	guardRuntimeModelMappingOptions(t)
	repo := &upstreamBillingProbeSettingRepo{}
	settingsService := NewSettingService(repo, &config.Config{})

	settings, err := settingsService.GetCodexAutoDegradationCheckSettings(context.Background())
	require.NoError(t, err)
	require.False(t, settings.Enabled, "feature must be OFF by default (no surprise upstream traffic)")
	require.Equal(t, 10, settings.IntervalMinutes)
	require.Equal(t, 30, settings.BlockMinutes)
	require.Equal(t, []string{"gpt-6.1-sol"}, settings.Models)

	// parseSettings（GET 回显 / 保存合并）在 key 缺失时同样回填默认值。
	parsed := settingsService.parseSettings(map[string]string{})
	require.NotNil(t, parsed.CodexAutoDegradationCheck)
	require.Equal(t, settings, parsed.CodexAutoDegradationCheck)
}

func TestCodexAutoDegradationCheckSettingsRoundTrip(t *testing.T) {
	repo := &upstreamBillingProbeSettingRepo{}
	settingsService := NewSettingService(repo, &config.Config{})
	ctx := context.Background()

	want := &CodexAutoDegradationCheckSettings{
		Enabled:         true,
		IntervalMinutes: 7,
		BlockMinutes:    45,
		Models:          []string{"gpt-6.1-sol"},
	}
	updates, err := settingsService.buildSystemSettingsUpdates(ctx, &SystemSettings{CodexAutoDegradationCheck: want})
	require.NoError(t, err)
	raw, ok := updates[SettingKeyCodexAutoDegradationCheck]
	require.True(t, ok, "non-nil settings must be written")
	require.NoError(t, settingsService.settingRepo.Set(ctx, SettingKeyCodexAutoDegradationCheck, raw))

	got, err := settingsService.GetCodexAutoDegradationCheckSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, want, got)

	// Get 回读值与存储 JSON 一致（真正的存储→读取往返）。
	var stored CodexAutoDegradationCheckSettings
	require.NoError(t, json.Unmarshal([]byte(raw), &stored))
	require.Equal(t, *want, stored)

	// nil = 请求未携带该配置：不写 key，保留既有值。
	updates, err = settingsService.buildSystemSettingsUpdates(ctx, &SystemSettings{})
	require.NoError(t, err)
	_, present := updates[SettingKeyCodexAutoDegradationCheck]
	require.False(t, present, "nil settings must not touch the stored value")
}

func TestCodexAutoDegradationCheckSettingsValidation(t *testing.T) {
	repo := &upstreamBillingProbeSettingRepo{}
	settingsService := NewSettingService(repo, &config.Config{})
	ctx := context.Background()

	valid := &CodexAutoDegradationCheckSettings{
		Enabled:         true,
		IntervalMinutes: 10,
		BlockMinutes:    30,
		Models:          []string{"gpt-6.1-sol"},
	}
	require.NoError(t, ValidateCodexAutoDegradationCheckSettings(valid))

	badInterval := *valid
	badInterval.IntervalMinutes = 0
	_, err := settingsService.buildSystemSettingsUpdates(ctx, &SystemSettings{CodexAutoDegradationCheck: &badInterval})
	require.ErrorContains(t, err, "interval_minutes must be at least 1")

	badBlock := *valid
	badBlock.BlockMinutes = 0
	_, err = settingsService.buildSystemSettingsUpdates(ctx, &SystemSettings{CodexAutoDegradationCheck: &badBlock})
	require.ErrorContains(t, err, "block_minutes must be at least 1")

	unknownModel := *valid
	unknownModel.Models = []string{"gpt-9-does-not-exist"}
	_, err = settingsService.buildSystemSettingsUpdates(ctx, &SystemSettings{CodexAutoDegradationCheck: &unknownModel})
	require.ErrorContains(t, err, "unknown diagnostic model")

	duplicate := *valid
	duplicate.Models = []string{"gpt-6.1-sol", "gpt-6.1-sol"}
	_, err = settingsService.buildSystemSettingsUpdates(ctx, &SystemSettings{CodexAutoDegradationCheck: &duplicate})
	require.ErrorContains(t, err, "duplicate diagnostic model")

	empty := *valid
	empty.Models = []string{}
	_, err = settingsService.buildSystemSettingsUpdates(ctx, &SystemSettings{CodexAutoDegradationCheck: &empty})
	require.ErrorContains(t, err, "models must not be empty")

	// 被拒绝的配置绝不落库。
	require.NotContains(t, repo.values, SettingKeyCodexAutoDegradationCheck)
}

func TestCodexAutoDegradationCheckSettingsParseCorruptionFallsBackToDefaults(t *testing.T) {
	guardRuntimeModelMappingOptions(t)
	repo := &upstreamBillingProbeSettingRepo{values: map[string]string{
		SettingKeyCodexAutoDegradationCheck: `{"enabled":`,
	}}
	settingsService := NewSettingService(repo, &config.Config{})
	parsed := settingsService.parseSettings(repo.values)
	require.Equal(t, DefaultCodexAutoDegradationCheckSettings(), parsed.CodexAutoDegradationCheck)
}

// ---------------------------------------------------------------------------
// service state machine
// ---------------------------------------------------------------------------

func TestCodexAutoDegradationCheckHealthyProbe(t *testing.T) {
	base := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	account := activeOpenAIOAuthAccount(1)
	repo := newCodexDegradationRepoStub(account)
	probe := fixedVerdict("normal")
	svc := newCodexDegradationTestService(repo, probe, enabledDegradationSettings())
	svc.now = func() time.Time { return base }

	settings, err := svc.getSettings(context.Background())
	require.NoError(t, err)
	require.NoError(t, svc.runAccountCheck(context.Background(), account.ID, settings))

	require.Empty(t, repo.setCalls, "healthy probe must not block")
	require.Empty(t, repo.clearCalls, "nothing to clear")
	require.Equal(t, 1, probe.callCount())

	state := repo.storedState(t, account.ID)
	require.Equal(t, "normal", state.Status)
	require.Equal(t, base, state.CheckedAt)
	require.Equal(t, base.Add(10*time.Minute), state.NextCheckAt)
	require.Nil(t, state.BlockedUntil)
	require.Equal(t, []string{"gpt-6.1-sol"}, state.Models)
}

func TestCodexAutoDegradationCheckDegradedBlocksAndRechecksAfterBlock(t *testing.T) {
	base := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	account := activeOpenAIOAuthAccount(1)
	repo := newCodexDegradationRepoStub(account)
	probe := fixedVerdict("degraded")
	svc := newCodexDegradationTestService(repo, probe, enabledDegradationSettings())
	now := base
	svc.now = func() time.Time { return now }

	settings, err := svc.getSettings(context.Background())
	require.NoError(t, err)
	require.NoError(t, svc.runAccountCheck(context.Background(), account.ID, settings))

	require.Len(t, repo.setCalls, 1)
	first := repo.setCalls[0]
	require.Equal(t, base.Add(30*time.Minute), first.until)
	require.True(t, IsCodexAutoDegradationCheckReason(first.reason), "block reason must carry the feature marker")
	require.Equal(t, account.ID, first.accountID)
	require.Empty(t, repo.clearCalls)

	state := repo.storedState(t, account.ID)
	require.Equal(t, "degraded", state.Status)
	require.Equal(t, base.Add(30*time.Minute), state.NextCheckAt, "re-test exactly after the block expires")
	require.NotNil(t, state.BlockedUntil)
	require.Equal(t, base.Add(30*time.Minute), *state.BlockedUntil)

	// 封锁到点后复查：仍然降级 → 封锁续期 30 分钟，next_check_at 再推 30 分钟。
	now = base.Add(30 * time.Minute)
	require.NoError(t, svc.runAccountCheck(context.Background(), account.ID, settings))

	require.Len(t, repo.setCalls, 2)
	second := repo.setCalls[1]
	require.Equal(t, base.Add(60*time.Minute), second.until)
	require.True(t, IsCodexAutoDegradationCheckReason(second.reason))
	repo.mu.Lock()
	require.NotNil(t, account.TempUnschedulableUntil)
	require.Equal(t, base.Add(60*time.Minute), *account.TempUnschedulableUntil, "block must be extended while still degraded")
	repo.mu.Unlock()

	state = repo.storedState(t, account.ID)
	require.Equal(t, base.Add(60*time.Minute), state.NextCheckAt)
	require.Equal(t, 2, probe.callCount())
}

func TestCodexAutoDegradationCheckHealthyClearsOnlyOwnBlocks(t *testing.T) {
	base := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)

	t.Run("marker reason is cleared", func(t *testing.T) {
		account := activeOpenAIOAuthAccount(1)
		blockedUntil := base.Add(5 * time.Minute)
		account.TempUnschedulableUntil = &blockedUntil
		account.TempUnschedulableReason = `{"source":"auto_degradation_check","status":"degraded","models":["gpt-6.1-sol"],"until_unix":1790000000,"error_message":"auto degradation check: degraded"}`
		repo := newCodexDegradationRepoStub(account)
		probe := fixedVerdict("normal")
		svc := newCodexDegradationTestService(repo, probe, enabledDegradationSettings())
		svc.now = func() time.Time { return base }

		settings, err := svc.getSettings(context.Background())
		require.NoError(t, err)
		require.NoError(t, svc.runAccountCheck(context.Background(), account.ID, settings))

		require.Equal(t, []int64{account.ID}, repo.clearCalls, "own block must be cleared on healthy probe")
		repo.mu.Lock()
		require.Nil(t, account.TempUnschedulableUntil)
		require.Empty(t, account.TempUnschedulableReason)
		repo.mu.Unlock()
		require.Empty(t, repo.setCalls)

		state := repo.storedState(t, account.ID)
		require.Equal(t, "normal", state.Status)
		require.Nil(t, state.BlockedUntil)
		require.Equal(t, base.Add(10*time.Minute), state.NextCheckAt)
	})

	t.Run("foreign reason survives healthy probe", func(t *testing.T) {
		for _, reason := range []string{
			`{"status_code":503,"until_unix":1790000000}`,
			"OAuth 401: unauthorized",
			"account_scheduling_threshold",
		} {
			account := activeOpenAIOAuthAccount(1)
			blockedUntil := base.Add(20 * time.Minute)
			account.TempUnschedulableUntil = &blockedUntil
			account.TempUnschedulableReason = reason
			repo := newCodexDegradationRepoStub(account)
			probe := fixedVerdict("uncertain")
			svc := newCodexDegradationTestService(repo, probe, enabledDegradationSettings())
			svc.now = func() time.Time { return base }

			settings, err := svc.getSettings(context.Background())
			require.NoError(t, err)
			require.NoError(t, svc.runAccountCheck(context.Background(), account.ID, settings))

			require.Empty(t, repo.clearCalls, "a block written by another subsystem must survive: %s", reason)
			repo.mu.Lock()
			require.NotNil(t, account.TempUnschedulableUntil)
			require.Equal(t, reason, account.TempUnschedulableReason)
			repo.mu.Unlock()

			state := repo.storedState(t, account.ID)
			require.Equal(t, "uncertain", state.Status, "uncertain counts as healthy")
			require.Equal(t, base.Add(10*time.Minute), state.NextCheckAt)
		}
	})
}

func TestCodexAutoDegradationCheckFailedProbeLeavesStateUntouched(t *testing.T) {
	base := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	foreignReason := `{"status_code":503,"until_unix":1790000000}`

	cases := map[string]codexAutoDegradationProbe{
		"failed verdict": fixedVerdict("failed"),
		"probe error": &codexDegradationProbeStub{next: func(_ int64, _ string) (CodexDiagnosticVerdict, error) {
			return CodexDiagnosticVerdict{}, errProbeBoom
		}},
	}
	for name, probe := range cases {
		t.Run(name, func(t *testing.T) {
			account := activeOpenAIOAuthAccount(1)
			// 预置一条其它子系统写的封锁：失败探测既不能解它，也不能覆盖它。
			blockedUntil := base.Add(20 * time.Minute)
			account.TempUnschedulableUntil = &blockedUntil
			account.TempUnschedulableReason = foreignReason
			repo := newCodexDegradationRepoStub(account)
			svc := newCodexDegradationTestService(repo, probe, enabledDegradationSettings())
			svc.now = func() time.Time { return base }

			settings, err := svc.getSettings(context.Background())
			require.NoError(t, err)
			require.NoError(t, svc.runAccountCheck(context.Background(), account.ID, settings))

			require.Empty(t, repo.setCalls, "failed probe must never create a block")
			require.Empty(t, repo.clearCalls, "failed probe must never clear a block")
			repo.mu.Lock()
			require.NotNil(t, account.TempUnschedulableUntil)
			require.Equal(t, foreignReason, account.TempUnschedulableReason)
			repo.mu.Unlock()

			state := repo.storedState(t, account.ID)
			require.Equal(t, "failed", state.Status)
			require.Equal(t, base.Add(10*time.Minute), state.NextCheckAt, "retry after the normal interval")
		})
	}
}

func TestCodexAutoDegradationCheckSkipsAccountsBeforeNextCheck(t *testing.T) {
	base := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	account := activeOpenAIOAuthAccount(1)
	account.Extra = map[string]any{
		CodexAutoDegradationCheckExtraKey: &CodexAutoDegradationCheckState{
			Status:      "normal",
			CheckedAt:   base.Add(-5 * time.Minute),
			NextCheckAt: base.Add(5 * time.Minute),
		},
	}
	repo := newCodexDegradationRepoStub(account)
	probe := fixedVerdict("degraded")
	svc := newCodexDegradationTestService(repo, probe, enabledDegradationSettings())
	svc.now = func() time.Time { return base }

	settings, err := svc.getSettings(context.Background())
	require.NoError(t, err)
	require.NoError(t, svc.runAccountCheck(context.Background(), account.ID, settings))

	require.Equal(t, 0, probe.callCount(), "not yet due → no probe")
	require.Empty(t, repo.setCalls)
	require.Empty(t, repo.clearCalls)
	require.Empty(t, repo.extraUpdates)
}

func TestCodexAutoDegradationCheckRunDueGatesOnEnabledAndEligibility(t *testing.T) {
	base := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	eligible := activeOpenAIOAuthAccount(1)
	ineligible := &Account{ID: 2, Platform: PlatformAnthropic, Type: AccountTypeOAuth, Status: StatusActive}
	setupToken := activeOpenAIOAuthAccount(3)
	setupToken.Type = AccountTypeSetupToken

	t.Run("disabled runs nothing", func(t *testing.T) {
		repo := newCodexDegradationRepoStub(eligible, ineligible, setupToken)
		repo.dueAccounts = []Account{*eligible, *ineligible, *setupToken}
		probe := fixedVerdict("degraded")
		svc := newCodexDegradationTestService(repo, probe, map[string]string{
			SettingKeyCodexAutoDegradationCheck: `{"enabled":false,"interval_minutes":10,"block_minutes":30,"models":["gpt-6.1-sol"]}`,
		})
		svc.now = func() time.Time { return base }

		require.NoError(t, svc.RunDue(context.Background()))
		require.Equal(t, 0, probe.callCount())
		require.Equal(t, 0, repo.listCallCount)
	})

	t.Run("enabled probes only eligible accounts", func(t *testing.T) {
		repo := newCodexDegradationRepoStub(eligible, ineligible, setupToken)
		repo.dueAccounts = []Account{*eligible, *ineligible, *setupToken}
		probe := fixedVerdict("normal")
		svc := newCodexDegradationTestService(repo, probe, enabledDegradationSettings())
		svc.now = func() time.Time { return base }

		require.NoError(t, svc.RunDue(context.Background()))
		require.Equal(t, 2, probe.callCount(), "OpenAI oauth + setup-token only")
		require.NotContains(t, probe.probedIDs(), int64(2), "non-OpenAI account must not be probed")
		require.Equal(t, 1, repo.listCallCount)
		// 首次运行立即到检：两个 eligible 账号都写入了 next_check_at。
		require.Equal(t, base.Add(10*time.Minute), repo.storedState(t, 1).NextCheckAt)
		require.Equal(t, base.Add(10*time.Minute), repo.storedState(t, 3).NextCheckAt)
		require.False(t, repo.hasStoredState(t, 2), "ineligible account must stay untouched")
	})
}

func TestCodexAutoDegradationCheckProbeModelsCombination(t *testing.T) {
	svc := &CodexAutoDegradationCheckService{}
	verdicts := map[string]string{}
	probe := &codexDegradationProbeStub{
		next: func(_ int64, model string) (CodexDiagnosticVerdict, error) {
			return CodexDiagnosticVerdict{Status: verdicts[model]}, nil
		},
	}
	svc.probe = probe

	// 任一 degraded → degraded（阳性证据优先）。
	verdicts = map[string]string{"a": "degraded", "b": "failed"}
	status, reason, _ := svc.probeModels(context.Background(), 1, []string{"a", "b"})
	require.Equal(t, "degraded", status)
	require.Contains(t, reason, "a")

	// 无 degraded 但有 failed → failed（不封锁、不解封）。
	verdicts = map[string]string{"a": "normal", "b": "failed"}
	status, _, _ = svc.probeModels(context.Background(), 1, []string{"a", "b"})
	require.Equal(t, "failed", status)

	// 全部 normal/uncertain → 健康。
	verdicts = map[string]string{"a": "normal", "b": "uncertain"}
	status, _, _ = svc.probeModels(context.Background(), 1, []string{"a", "b"})
	require.Equal(t, "uncertain", status)

	verdicts = map[string]string{"a": "normal", "b": "normal"}
	status, _, _ = svc.probeModels(context.Background(), 1, []string{"a", "b"})
	require.Equal(t, "normal", status)

	// 探针不可用 → failed，不产生任何封锁倾向。
	svc.probe = nil
	status, reason, results := svc.probeModels(context.Background(), 1, []string{"a"})
	require.Equal(t, "failed", status)
	require.Equal(t, "a: probe_unavailable", reason)
	require.Equal(t, []CodexAutoDegradationProbeResult{{Model: "a", Status: "failed", Reason: "probe_unavailable"}}, results)
}

func TestCodexAutoDegradationCheckPersistsProbeEvidence(t *testing.T) {
	base := time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC)
	account := activeOpenAIOAuthAccount(1)
	repo := newCodexDegradationRepoStub(account)
	models := []string{"gpt-6.1-sol", "gpt-6-astra", "partial", "request-error"}
	probe := &codexDegradationProbeStub{next: func(_ int64, model string) (CodexDiagnosticVerdict, error) {
		switch model {
		case "gpt-6.1-sol":
			return CodexDiagnosticVerdict{Status: "degraded", PredictedModel: "gpt-5.6", Probability: 0.875, ParsedCount: 64, Reason: "fingerprint_mismatch"}, nil
		case "gpt-6-astra":
			return CodexDiagnosticVerdict{Status: "normal", PredictedModel: "gpt-6-astra", Probability: 0.95, ParsedCount: 64}, nil
		case "partial":
			return CodexDiagnosticVerdict{Status: "failed", ParsedCount: 3, Reason: "insufficient_numbers"}, nil
		default:
			return CodexDiagnosticVerdict{}, errProbeBoom
		}
	}}
	svc := newCodexDegradationTestService(repo, probe, enabledDegradationSettings())
	svc.now = func() time.Time { return base }
	settings := &CodexAutoDegradationCheckSettings{Enabled: true, IntervalMinutes: 10, BlockMinutes: 30, Models: models}
	require.NoError(t, svc.runAccountCheck(context.Background(), account.ID, settings))

	// Assert the persisted JSON contract, including absence of invented predictions.
	rawState, err := json.Marshal(account.Extra)
	require.NoError(t, err)
	var stored map[string]any
	require.NoError(t, json.Unmarshal(rawState, &stored))
	state := decodeCodexAutoDegradationCheckState(stored)
	require.NotNil(t, state)
	require.Equal(t, "degraded", state.Status)
	expectedJSON := `[
		{"model":"gpt-6.1-sol","status":"degraded","predicted_model":"gpt-5.6","probability":0.875,"parsed_count":64,"reason":"fingerprint_mismatch"},
		{"model":"gpt-6-astra","status":"normal","predicted_model":"gpt-6-astra","probability":0.95,"parsed_count":64},
		{"model":"partial","status":"failed","parsed_count":3,"reason":"insufficient_numbers"},
		{"model":"request-error","status":"failed","reason":"boom"}
	]`
	var persistedState map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rawState, &persistedState))
	var stateFields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(persistedState[CodexAutoDegradationCheckExtraKey], &stateFields))
	require.JSONEq(t, expectedJSON, string(stateFields["probe_results"]))

	var reasonFields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(account.TempUnschedulableReason), &reasonFields))
	require.JSONEq(t, expectedJSON, string(reasonFields["probe_results"]))
	require.True(t, IsCodexAutoDegradationCheckReason(account.TempUnschedulableReason))
	display := tempUnschedStateFromStoredReason(account.TempUnschedulableReason, 0)
	require.Contains(t, display.ErrorMessage, "gpt-6.1-sol → gpt-5.6 (87.50%)")
	require.NotContains(t, display.ErrorMessage, "gpt-6-astra →", "healthy models must not be presented as degraded")
	require.Equal(t, base.Add(30*time.Minute).Unix(), display.UntilUnix)
}

func TestCodexAutoDegradationCheckCancellationRecordsUnattemptedModels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := &codexDegradationProbeStub{next: func(_ int64, _ string) (CodexDiagnosticVerdict, error) {
		cancel()
		return CodexDiagnosticVerdict{}, context.Canceled
	}}
	svc := &CodexAutoDegradationCheckService{probe: probe}
	status, _, results := svc.probeModels(ctx, 1, []string{"first", "second"})
	require.Equal(t, "failed", status)
	require.Equal(t, 1, probe.callCount(), "canceled context must not issue another request")
	require.Equal(t, []CodexAutoDegradationProbeResult{
		{Model: "first", Status: "failed", Reason: "context canceled"},
		{Model: "second", Status: "failed", Reason: "context canceled"},
	}, results)
}

func TestCodexAutoDegradationCheckPreservesLongerForeignBlock(t *testing.T) {
	base := time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC)
	account := activeOpenAIOAuthAccount(1)
	until := base.Add(time.Hour)
	foreignReason := `{"source":"temp_unschedulable_rule","status_code":503,"error_message":"overloaded"}`
	account.TempUnschedulableUntil = &until
	account.TempUnschedulableReason = foreignReason
	repo := newCodexDegradationRepoStub(account)
	svc := newCodexDegradationTestService(repo, fixedVerdict("degraded"), enabledDegradationSettings())
	svc.now = func() time.Time { return base }
	settings, err := svc.getSettings(context.Background())
	require.NoError(t, err)
	require.NoError(t, svc.runAccountCheck(context.Background(), account.ID, settings))
	require.Equal(t, &until, account.TempUnschedulableUntil)
	require.Equal(t, foreignReason, account.TempUnschedulableReason)
	require.Equal(t, "degraded", repo.storedState(t, account.ID).Status)
	require.Empty(t, repo.clearCalls)
}

func TestCodexAutoDegradationCheckStopIsIdempotent(t *testing.T) {
	repo := newCodexDegradationRepoStub()
	svc := newCodexDegradationTestService(repo, fixedVerdict("normal"), enabledDegradationSettings())
	require.NotPanics(t, func() {
		svc.Start()
		svc.Stop()
		svc.Stop()
	})
}
