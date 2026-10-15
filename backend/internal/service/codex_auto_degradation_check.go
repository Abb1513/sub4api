package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/MACOS-DO/sub4api/internal/pkg/errors"
	"github.com/MACOS-DO/sub4api/internal/pkg/logger"
	"github.com/google/uuid"
)

const (
	// CodexAutoDegradationCheckExtraKey 是 accounts.extra 中保存本功能
	// 每账号调度状态的 key：{"status","checked_at","next_check_at",
	// "blocked_until","models","reason","probe_results"}。
	CodexAutoDegradationCheckExtraKey = "codex_degradation_check"

	// CodexAutoDegradationCheckReasonSource 是写入 temp_unschedulable_reason
	// 的机器可读标记。只有携带该 source 的封锁才会被本功能自动解除，
	// 503 临时停调规则等其它子系统写入的封锁必须原样存活。
	CodexAutoDegradationCheckReasonSource = "auto_degradation_check"

	codexAutoDegradationCheckDefaultIntervalMinutes = 10
	codexAutoDegradationCheckDefaultBlockMinutes    = 30
	// 运行器扫描周期：以分钟粒度发现到期账号，具体间隔由设置决定。
	codexAutoDegradationCheckCycleInterval = time.Minute
	// 单轮最多处理的账号数：探测是账号原生的完整 ModelTrace 请求，
	// 顺序执行且每发有独立超时，必须限制单轮工作量。
	codexAutoDegradationCheckMaxPerRun = 8
	// 单发探测超时，与手动降智检测入口保持一致（120s）。
	codexAutoDegradationCheckProbeTimeout = 120 * time.Second
	// 单轮运行上下文上限：maxPerRun × probeTimeout 之外再留余量，
	// Stop() 取消父上下文时同样立刻生效，绝不泄漏 goroutine。
	codexAutoDegradationCheckRunTimeout    = 20 * time.Minute
	codexAutoDegradationCheckLeaderLockKey = "codex:auto_degradation:check:leader"
	codexAutoDegradationCheckLeaderLockTTL = 2 * time.Minute

	codexAutoDegradationStatusNormal    = "normal"
	codexAutoDegradationStatusDegraded  = "degraded"
	codexAutoDegradationStatusUncertain = "uncertain"
	codexAutoDegradationStatusFailed    = "failed"
)

// CodexAutoDegradationCheckSettings 控制自动降智检测后台运行器。
// 默认关闭，避免意外产生上游探测流量。
type CodexAutoDegradationCheckSettings struct {
	Enabled         bool     `json:"enabled"`
	IntervalMinutes int      `json:"interval_minutes"`
	BlockMinutes    int      `json:"block_minutes"`
	Models          []string `json:"models"`
}

// DefaultCodexAutoDegradationCheckSettings 返回缺省配置：关闭、10 分钟一检、
// 降智封锁 30 分钟、默认检测模型 gpt-6.1-sol。
func DefaultCodexAutoDegradationCheckSettings() *CodexAutoDegradationCheckSettings {
	return &CodexAutoDegradationCheckSettings{
		Enabled:         false,
		IntervalMinutes: codexAutoDegradationCheckDefaultIntervalMinutes,
		BlockMinutes:    codexAutoDegradationCheckDefaultBlockMinutes,
		Models:          []string{"gpt-6.1-sol"},
	}
}

// ValidateCodexAutoDegradationCheckSettings 是保存路径的校验入口：
// interval/block ≥ 1；models 必须非空、无重复，且都是
// ModelTraceDiagnosticModels() 的子集——指纹库换代后失效的模型在保存时
// 直接拒绝并给出明确报错，而不是静默带病运行。
func ValidateCodexAutoDegradationCheckSettings(settings *CodexAutoDegradationCheckSettings) error {
	if settings == nil {
		return infraerrors.BadRequest("INVALID_CODEX_AUTO_DEGRADATION_CHECK_SETTINGS", "settings cannot be nil")
	}
	if settings.IntervalMinutes < 1 {
		return infraerrors.BadRequest("INVALID_CODEX_AUTO_DEGRADATION_CHECK_INTERVAL", "interval_minutes must be at least 1")
	}
	if settings.BlockMinutes < 1 {
		return infraerrors.BadRequest("INVALID_CODEX_AUTO_DEGRADATION_CHECK_BLOCK", "block_minutes must be at least 1")
	}
	if len(settings.Models) == 0 {
		return infraerrors.BadRequest("INVALID_CODEX_AUTO_DEGRADATION_CHECK_MODELS", "models must not be empty")
	}
	allowed, err := ModelTraceDiagnosticModels()
	if err != nil {
		return fmt.Errorf("load diagnostic model list: %w", err)
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, model := range allowed {
		allowedSet[model] = struct{}{}
	}
	seen := make(map[string]struct{}, len(settings.Models))
	for _, model := range settings.Models {
		if _, ok := allowedSet[model]; !ok {
			return infraerrors.BadRequest(
				"INVALID_CODEX_AUTO_DEGRADATION_CHECK_MODELS",
				fmt.Sprintf("unknown diagnostic model %q", model),
			)
		}
		if _, dup := seen[model]; dup {
			return infraerrors.BadRequest(
				"INVALID_CODEX_AUTO_DEGRADATION_CHECK_MODELS",
				fmt.Sprintf("duplicate diagnostic model %q", model),
			)
		}
		seen[model] = struct{}{}
	}
	return nil
}

// GetCodexAutoDegradationCheckSettings 读取运行器配置；key 缺失时返回默认值
// （enabled=false），字段缺失/非法时逐项回退默认，保证运行器永远拿到可用配置。
func (s *SettingService) GetCodexAutoDegradationCheckSettings(ctx context.Context) (*CodexAutoDegradationCheckSettings, error) {
	defaults := DefaultCodexAutoDegradationCheckSettings()
	if s == nil || s.settingRepo == nil {
		return defaults, nil
	}
	value, err := s.settingRepo.GetValue(ctx, SettingKeyCodexAutoDegradationCheck)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			return defaults, nil
		}
		return nil, fmt.Errorf("get codex auto degradation check settings: %w", err)
	}
	if strings.TrimSpace(value) == "" {
		return defaults, nil
	}
	settings := *defaults
	if err := json.Unmarshal([]byte(value), &settings); err != nil {
		return nil, fmt.Errorf("parse codex auto degradation check settings: %w", err)
	}
	normalizeCodexAutoDegradationCheckSettings(&settings)
	return &settings, nil
}

func normalizeCodexAutoDegradationCheckSettings(settings *CodexAutoDegradationCheckSettings) {
	defaults := DefaultCodexAutoDegradationCheckSettings()
	if settings.IntervalMinutes < 1 {
		settings.IntervalMinutes = defaults.IntervalMinutes
	}
	if settings.BlockMinutes < 1 {
		settings.BlockMinutes = defaults.BlockMinutes
	}
	if len(settings.Models) == 0 {
		settings.Models = defaults.Models
	}
}

// CodexAutoDegradationProbeResult 保留请求模型与该次探测的实际判定。
// 未获得预测时省略预测模型和概率，解析失败仍可保留实际已解析数量。
type CodexAutoDegradationProbeResult struct {
	Model          string   `json:"model"`
	Status         string   `json:"status"`
	PredictedModel string   `json:"predicted_model,omitempty"`
	Probability    *float64 `json:"probability,omitempty"`
	ParsedCount    *int     `json:"parsed_count,omitempty"`
	Reason         string   `json:"reason,omitempty"`
}

// CodexAutoDegradationCheckState 是保存在 accounts.extra 中的每账号调度状态。
type CodexAutoDegradationCheckState struct {
	Status       string                            `json:"status"`
	CheckedAt    time.Time                         `json:"checked_at"`
	NextCheckAt  time.Time                         `json:"next_check_at"`
	BlockedUntil *time.Time                        `json:"blocked_until,omitempty"`
	Models       []string                          `json:"models"`
	Reason       string                            `json:"reason,omitempty"`
	ProbeResults []CodexAutoDegradationProbeResult `json:"probe_results,omitempty"`
}

// decodeCodexAutoDegradationCheckState 兼容两种载体：DB 读回的 map（JSON）
// 与测试/内存路径直接写入的结构体指针；任何解析失败都视作“无状态”（首次运行）。
func decodeCodexAutoDegradationCheckState(extra map[string]any) *CodexAutoDegradationCheckState {
	if extra == nil {
		return nil
	}
	raw, ok := extra[CodexAutoDegradationCheckExtraKey]
	if !ok || raw == nil {
		return nil
	}
	switch typed := raw.(type) {
	case *CodexAutoDegradationCheckState:
		return typed
	case CodexAutoDegradationCheckState:
		return &typed
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var state CodexAutoDegradationCheckState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil
	}
	return &state
}

// codexAutoDegradationCheckReasonPayload 是写入 temp_unschedulable_reason 的
// 机器可读封锁原因。source 是本功能的识别标记；until_unix/error_message 与
// 既有 TempUnsched 展示链路（tempUnschedStateFromStoredReason）字段对齐。
type codexAutoDegradationCheckReasonPayload struct {
	Source       string                            `json:"source"`
	Status       string                            `json:"status"`
	Models       []string                          `json:"models"`
	ProbeResults []CodexAutoDegradationProbeResult `json:"probe_results,omitempty"`
	UntilUnix    int64                             `json:"until_unix"`
	ErrorMessage string                            `json:"error_message"`
}

func buildCodexAutoDegradationCheckReason(status string, models []string, until time.Time, results []CodexAutoDegradationProbeResult) string {
	message := fmt.Sprintf("auto degradation check: %s", status)
	var details []string
	for _, result := range results {
		if result.Status != codexAutoDegradationStatusDegraded || result.PredictedModel == "" {
			continue
		}
		detail := result.Model + " → " + result.PredictedModel
		if result.Probability != nil {
			detail += fmt.Sprintf(" (%.2f%%)", *result.Probability*100)
		}
		details = append(details, detail)
	}
	if len(details) > 0 {
		message += ": " + strings.Join(details, "; ")
	}
	payload := codexAutoDegradationCheckReasonPayload{
		Source:       CodexAutoDegradationCheckReasonSource,
		Status:       status,
		Models:       models,
		ProbeResults: results,
		UntilUnix:    until.UTC().Unix(),
		ErrorMessage: message,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return BuildTempUnschedReasonPayload(CodexAutoDegradationCheckReasonSource, payload.ErrorMessage)
	}
	return string(raw)
}

// IsCodexAutoDegradationCheckReason 报告一条 temp_unschedulable_reason 是否
// 由本功能写入。只有带标记的封锁才允许被健康探测自动解除。
func IsCodexAutoDegradationCheckReason(rawReason string) bool {
	payload, ok := parseTempUnschedReasonPayload(rawReason)
	return ok && payload.Source == CodexAutoDegradationCheckReasonSource
}

// codexAutoDegradationProbe 以账号自身的上游凭据执行一发降智探测并给出判定。
// 抽成接口是为了让状态机测试不触网。
type codexAutoDegradationProbe interface {
	ProbeCodexAutoDegradation(ctx context.Context, accountID int64, model string) (CodexDiagnosticVerdict, error)
}

// codexAutoDegradationGatewayProbe 是生产实现：走 OpenAIGatewayService 的
// 号原生 ModelTrace 探测 + 统一评分。
type codexAutoDegradationGatewayProbe struct {
	gateway *OpenAIGatewayService
	timeout time.Duration
}

func (p *codexAutoDegradationGatewayProbe) ProbeCodexAutoDegradation(ctx context.Context, accountID int64, model string) (CodexDiagnosticVerdict, error) {
	if p == nil || p.gateway == nil {
		return CodexDiagnosticVerdict{Status: codexAutoDegradationStatusFailed}, errors.New("codex degradation probe unavailable")
	}
	challenge, err := NewModelTraceChallenge()
	if err != nil {
		return CodexDiagnosticVerdict{Status: codexAutoDegradationStatusFailed}, fmt.Errorf("generate model trace challenge: %w", err)
	}
	result := p.gateway.RunCodexNativeDiagnostic(ctx, accountID, model, challenge, p.timeout)
	if result.Reason != "" {
		// 请求/HTTP/输出不可评分：按探测失败处理，绝不能当作降智或健康。
		return CodexDiagnosticVerdict{Status: codexAutoDegradationStatusFailed, Reason: result.Reason}, nil
	}
	return EvaluateCodexDiagnostic(model, result.Output, challenge), nil
}

// CodexAutoDegradationCheckService 是“自动降智检测”后台运行器：
// 每个到期的 OpenAI oauth/setup-token 活跃账号顺序执行账号原生探测，
// degraded → 临时停调 block_minutes（到点复查并续期），
// normal/uncertain → 解除本功能写入的封锁并按 interval_minutes 复检，
// failed → 不封锁、不解封，按 interval_minutes 重试。
type CodexAutoDegradationCheckService struct {
	accountRepo      AccountRepository
	settingService   *SettingService
	rateLimitService *RateLimitService
	probe            codexAutoDegradationProbe

	parentCtx    context.Context
	parentCancel context.CancelFunc
	wg           sync.WaitGroup
	mu           sync.Mutex
	started      bool
	stopped      bool
	cycleMu      sync.Mutex
	now          func() time.Time
	lockCache    LeaderLockCache
	db           *sql.DB
	instanceID   string
}

type codexAutoDegradationCheckDueAccountLister interface {
	ListDueCodexAutoDegradationCheckAccounts(ctx context.Context, now time.Time, limit int) ([]Account, error)
}

func NewCodexAutoDegradationCheckService(
	accountRepo AccountRepository,
	settingService *SettingService,
	rateLimitService *RateLimitService,
	probe codexAutoDegradationProbe,
) *CodexAutoDegradationCheckService {
	ctx, cancel := context.WithCancel(context.Background())
	return &CodexAutoDegradationCheckService{
		accountRepo:      accountRepo,
		settingService:   settingService,
		rateLimitService: rateLimitService,
		probe:            probe,
		parentCtx:        ctx,
		parentCancel:     cancel,
		now:              time.Now,
		instanceID:       uuid.NewString(),
	}
}

func (s *CodexAutoDegradationCheckService) SetLeaderLock(lockCache LeaderLockCache, db *sql.DB) {
	if s == nil {
		return
	}
	s.lockCache = lockCache
	s.db = db
}

// ProvideCodexAutoDegradationCheckService 构造并启动进程级周期运行器。
func ProvideCodexAutoDegradationCheckService(
	gateway *OpenAIGatewayService,
	accountRepo AccountRepository,
	settingService *SettingService,
	rateLimitService *RateLimitService,
	lockCache LeaderLockCache,
	db *sql.DB,
) *CodexAutoDegradationCheckService {
	probe := &codexAutoDegradationGatewayProbe{gateway: gateway, timeout: codexAutoDegradationCheckProbeTimeout}
	svc := NewCodexAutoDegradationCheckService(accountRepo, settingService, rateLimitService, probe)
	svc.SetLeaderLock(lockCache, db)
	svc.Start()
	return svc
}

func (s *CodexAutoDegradationCheckService) Start() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.started || s.stopped {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.wg.Add(1)
	s.mu.Unlock()
	go s.runLoop()
}

func (s *CodexAutoDegradationCheckService) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	s.parentCancel()
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *CodexAutoDegradationCheckService) runLoop() {
	defer s.wg.Done()
	_ = s.RunDue(s.parentCtx)
	ticker := time.NewTicker(codexAutoDegradationCheckCycleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.parentCtx.Done():
			return
		case <-ticker.C:
			if err := s.RunDue(s.parentCtx); err != nil {
				logger.LegacyPrintf("service.codex_auto_degradation_check", "run_due_failed: err=%v", err)
			}
		}
	}
}

// RunDue 执行一轮到期账号检查；全程顺序执行，配合 cycleMu 保证同一账号
// 任何时刻至多一发探测在飞。
func (s *CodexAutoDegradationCheckService) RunDue(ctx context.Context) error {
	if s == nil || s.accountRepo == nil {
		return nil
	}
	s.cycleMu.Lock()
	defer s.cycleMu.Unlock()

	settings, err := s.getSettings(ctx)
	if err != nil {
		return err
	}
	if !settings.Enabled {
		return nil
	}
	runRelease, acquired, lockErr := s.tryAcquireLeaderLock(ctx, codexAutoDegradationCheckLeaderLockKey)
	if lockErr != nil {
		return fmt.Errorf("acquire codex auto degradation check leader lock: %w", lockErr)
	}
	if !acquired {
		return nil
	}
	defer runRelease()

	lockNow := s.currentTime()
	cadenceKey := codexAutoDegradationCheckLeaderLockKey + ":" + strconv.FormatInt(lockNow.Unix()/int64(codexAutoDegradationCheckCycleInterval/time.Second), 10)
	cadenceRelease, acquired, lockErr := s.tryAcquireLeaderLock(ctx, cadenceKey)
	if lockErr != nil {
		return fmt.Errorf("acquire codex auto degradation check cadence lock: %w", lockErr)
	}
	if !acquired {
		return nil
	}
	defer cadenceRelease()

	now := s.currentTime()
	accounts, err := s.listDueAccounts(ctx, now)
	if err != nil {
		return fmt.Errorf("list due codex auto degradation check accounts: %w", err)
	}
	due := make([]Account, 0, len(accounts))
	for i := range accounts {
		account := accounts[i]
		if !codexAutoDegradationCheckEligible(&account) || !account.IsActive() {
			continue
		}
		state := decodeCodexAutoDegradationCheckState(account.Extra)
		if state != nil && !state.NextCheckAt.IsZero() && now.Before(state.NextCheckAt) {
			continue
		}
		due = append(due, account)
	}
	sort.SliceStable(due, func(i, j int) bool {
		left := decodeCodexAutoDegradationCheckState(due[i].Extra)
		right := decodeCodexAutoDegradationCheckState(due[j].Extra)
		leftUnset := left == nil || left.NextCheckAt.IsZero()
		rightUnset := right == nil || right.NextCheckAt.IsZero()
		if leftUnset && rightUnset {
			return due[i].ID < due[j].ID
		}
		if leftUnset {
			return true
		}
		if rightUnset {
			return false
		}
		return left.NextCheckAt.Before(right.NextCheckAt)
	})
	if len(due) > codexAutoDegradationCheckMaxPerRun {
		due = due[:codexAutoDegradationCheckMaxPerRun]
	}

	runCtx, cancel := context.WithTimeout(ctx, codexAutoDegradationCheckRunTimeout)
	defer cancel()
	for i := range due {
		if runCtx.Err() != nil {
			break
		}
		if err := s.runAccountCheck(runCtx, due[i].ID, settings); err != nil {
			logger.LegacyPrintf("service.codex_auto_degradation_check", "account_check_failed: account_id=%d err=%v", due[i].ID, err)
		}
	}
	return nil
}

// codexAutoDegradationCheckEligible 即诊断面板使用的账号口径：
// OpenAI 平台 + oauth/setup-token 类型。
func codexAutoDegradationCheckEligible(account *Account) bool {
	return account != nil && account.IsOpenAIOAuthLike()
}

func (s *CodexAutoDegradationCheckService) listDueAccounts(ctx context.Context, now time.Time) ([]Account, error) {
	lister, ok := s.accountRepo.(codexAutoDegradationCheckDueAccountLister)
	if !ok {
		return nil, errors.New("account repository does not list due codex auto degradation check accounts")
	}
	return lister.ListDueCodexAutoDegradationCheckAccounts(ctx, now, codexAutoDegradationCheckMaxPerRun)
}

func (s *CodexAutoDegradationCheckService) getSettings(ctx context.Context) (*CodexAutoDegradationCheckSettings, error) {
	if s.settingService == nil {
		return DefaultCodexAutoDegradationCheckSettings(), nil
	}
	return s.settingService.GetCodexAutoDegradationCheckSettings(ctx)
}

func (s *CodexAutoDegradationCheckService) currentTime() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// runAccountCheck 对单个账号执行一次完整的“探测 → 判定 → 状态迁移”。
// 状态机约定（见需求）：
//   - 尚未到期（含未来 next_check_at）→ 直接跳过，不产生任何写入；
//   - degraded → SetTempUnschedulable(now+block_minutes, 带标记 reason)，
//     next_check_at = now+block_minutes，复查时仍降级则续期；
//   - normal/uncertain → 若当前封锁 reason 带本功能标记则解除，
//     next_check_at = now+interval_minutes；
//   - failed（请求/HTTP/评分失败）→ 不封锁、不解封，next_check_at = now+interval_minutes。
func (s *CodexAutoDegradationCheckService) runAccountCheck(ctx context.Context, accountID int64, settings *CodexAutoDegradationCheckSettings) error {
	if s == nil || s.accountRepo == nil || settings == nil {
		return nil
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return fmt.Errorf("load account: %w", err)
	}
	if account == nil || !codexAutoDegradationCheckEligible(account) || !account.IsActive() {
		return nil
	}
	now := s.currentTime().UTC()
	previous := decodeCodexAutoDegradationCheckState(account.Extra)
	if previous != nil && !previous.NextCheckAt.IsZero() && now.Before(previous.NextCheckAt) {
		return nil
	}
	models := append([]string(nil), settings.Models...)
	if len(models) == 0 {
		models = DefaultCodexAutoDegradationCheckSettings().Models
	}
	interval := time.Duration(settings.IntervalMinutes) * time.Minute

	status, reason, results := s.probeModels(ctx, accountID, models)
	state := &CodexAutoDegradationCheckState{
		Status:       status,
		CheckedAt:    now,
		Models:       models,
		Reason:       reason,
		ProbeResults: results,
	}
	if previous != nil {
		state.BlockedUntil = previous.BlockedUntil
	}

	switch status {
	case codexAutoDegradationStatusDegraded:
		until := now.Add(time.Duration(settings.BlockMinutes) * time.Minute)
		// SetTempUnschedulable 是单调只延长：已有更晚的封锁（例如 503 规则的
		// 长封锁）不会被缩短，reason 也不会被覆盖，标记语义保持成立。
		if err := s.accountRepo.SetTempUnschedulable(ctx, accountID, until, buildCodexAutoDegradationCheckReason(status, models, until, results)); err != nil {
			return fmt.Errorf("set temp unschedulable: %w", err)
		}
		state.BlockedUntil = &until
		state.NextCheckAt = until
	case codexAutoDegradationStatusFailed:
		// 探测失败绝不封锁、也绝不解除任何封锁。
		state.NextCheckAt = now.Add(interval)
	default: // normal / uncertain
		state.NextCheckAt = now.Add(interval)
		if account.TempUnschedulableUntil != nil && IsCodexAutoDegradationCheckReason(account.TempUnschedulableReason) {
			if s.rateLimitService == nil {
				return errors.New("rate limit service unavailable for clearing auto degradation block")
			}
			// 用 RateLimitService 清除：它同时清掉 Redis 临时停调缓存与
			// 本功能从未写入的模型级限流，保证调度器立刻看到恢复。
			if err := s.rateLimitService.ClearTempUnschedulable(ctx, accountID); err != nil {
				return fmt.Errorf("clear temp unschedulable: %w", err)
			}
			state.BlockedUntil = nil
		}
	}
	return s.accountRepo.UpdateExtra(ctx, accountID, map[string]any{CodexAutoDegradationCheckExtraKey: state})
}

// probeModels 顺序探测全部配置模型并合并判定：
// 任一 degraded → degraded；否则任一 failed → failed；否则任一 uncertain →
// uncertain（与 normal 同属健康）；全 normal → normal。
func (s *CodexAutoDegradationCheckService) probeModels(ctx context.Context, accountID int64, models []string) (string, string, []CodexAutoDegradationProbeResult) {
	results := make([]CodexAutoDegradationProbeResult, 0, len(models))
	var degraded []string
	var failures []string
	uncertain := false
	for _, model := range models {
		var verdict CodexDiagnosticVerdict
		switch {
		case ctx.Err() != nil:
			verdict = CodexDiagnosticVerdict{Status: codexAutoDegradationStatusFailed, Reason: probeFailureMessage(ctx.Err())}
		case s == nil || s.probe == nil:
			verdict = CodexDiagnosticVerdict{Status: codexAutoDegradationStatusFailed, Reason: "probe_unavailable"}
		default:
			var err error
			verdict, err = s.probe.ProbeCodexAutoDegradation(ctx, accountID, model)
			if err != nil {
				verdict = CodexDiagnosticVerdict{Status: codexAutoDegradationStatusFailed, Reason: probeFailureMessage(err)}
			}
		}
		result := CodexAutoDegradationProbeResult{
			Model:          model,
			Status:         verdict.Status,
			PredictedModel: verdict.PredictedModel,
			Reason:         verdict.Reason,
		}
		if verdict.PredictedModel != "" {
			result.Probability = &verdict.Probability
		}
		if verdict.PredictedModel != "" || verdict.ParsedCount > 0 {
			result.ParsedCount = &verdict.ParsedCount
		}
		switch verdict.Status {
		case codexAutoDegradationStatusDegraded:
			degraded = append(degraded, model)
		case codexAutoDegradationStatusUncertain:
			uncertain = true
		case codexAutoDegradationStatusNormal:
		default:
			result.Status = codexAutoDegradationStatusFailed
			if result.Reason == "" {
				result.Reason = codexAutoDegradationStatusFailed
			}
			failures = append(failures, fmt.Sprintf("%s: %s", model, result.Reason))
		}
		results = append(results, result)
	}
	if len(degraded) > 0 {
		return codexAutoDegradationStatusDegraded, "degraded models: " + strings.Join(degraded, ","), results
	}
	if len(failures) > 0 {
		return codexAutoDegradationStatusFailed, strings.Join(failures, ";"), results
	}
	if uncertain {
		return codexAutoDegradationStatusUncertain, "", results
	}
	return codexAutoDegradationStatusNormal, "", results
}

func probeFailureMessage(err error) string {
	message := strings.TrimSpace(err.Error())
	if message == "" {
		return "probe_failed"
	}
	return message
}

func (s *CodexAutoDegradationCheckService) tryAcquireLeaderLock(ctx context.Context, key string) (func(), bool, error) {
	lockCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if s.lockCache != nil {
		acquired, err := s.lockCache.TryAcquireLeaderLock(lockCtx, key, s.instanceID, codexAutoDegradationCheckLeaderLockTTL)
		if err != nil {
			return nil, false, err
		}
		if !acquired {
			return nil, false, nil
		}
		return func() {
			releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer releaseCancel()
			_ = s.lockCache.ReleaseLeaderLock(releaseCtx, key, s.instanceID)
		}, true, nil
	}
	if s.db != nil {
		return tryAcquireDBAdvisoryLockWithError(lockCtx, s.db, hashAdvisoryLockID(key))
	}
	return func() {}, true, nil
}
