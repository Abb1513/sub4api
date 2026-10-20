package migrations

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// expectedUserPlatformQuotaPlatforms 是 user_platform_quotas.platform 允许的平台集合，
// 需与 domain platform list 及 ent/schema/user_platform_quota.go 的 Validate 同步。
var expectedUserPlatformQuotaPlatforms = []string{
	"anthropic", "antigravity", "deepseek", "gemini", "grok", "kimi", "kiro",
	"minimax", "openai", "openai_bps", "opencode_go", "typesafe", "zhipu",
}

// TestUserPlatformQuotasRestoreKiroMigration 校验 227 号迁移把 224 漏掉的 kiro
// 重新加回 user_platform_quotas.platform 的 CHECK 约束。
//
// kiro 自 145 号迁移起就在约束内，224 号用 DROP + 重建的写法加国产 3 家时漏掉了它，
// 等于回退了 145。约束缺 kiro 时，注册预填充 9 平台默认配额会因该行违约中止整条
// INSERT → 快照 fail-open 仅 warn → 新用户零配额行（缺失配额行 = 无限额）。
func TestUserPlatformQuotasRestoreKiroMigration(t *testing.T) {
	content, err := FS.ReadFile("227_user_platform_quotas_restore_kiro.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check")
	require.Contains(t, sql,
		"CHECK (platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'kiro', 'grok', 'kimi', 'zhipu', 'deepseek'))")
}

// TestUserPlatformQuotaPlatformCheckFinalStateCoversAllPlatforms is a regression
// guard for the final platform list. Kiro must remain in the quota platform set.
func TestUserPlatformQuotaPlatformCheckFinalStateCoversAllPlatforms(t *testing.T) {
	got := append([]string{}, concretePlatformIDs()...)
	sort.Strings(got)

	require.Equal(t, expectedUserPlatformQuotaPlatforms, got,
		"domain platform list 与 user_platform_quotas.platform 允许的平台不一致；"+
			"新平台必须登记进 domain/platforms.go，漏项会让对应平台的配额行插入失败")
}
