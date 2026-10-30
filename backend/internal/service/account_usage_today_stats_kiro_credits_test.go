package service

import (
	"context"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
)

// Today stats feed the account list's credit display, so Kiro credits must
// survive the AccountStats -> WindowStats conversion on both batch paths.
func TestAccountUsageService_GetTodayStatsBatch_PreservesKiroCredits(t *testing.T) {
	stats := map[int64]*usagestats.AccountStats{
		7: {Requests: 3, Tokens: 900, Cost: 0.3, KiroCredits: 0.17},
	}

	t.Run("batch reader", func(t *testing.T) {
		svc := &AccountUsageService{usageLogRepo: &usageLogWindowBatchRepoStub{batchResult: stats}}
		result, err := svc.GetTodayStatsBatch(context.Background(), []int64{7, 8})
		require.NoError(t, err)
		require.InDelta(t, 0.17, result[7].KiroCredits, 1e-9)
		require.Zero(t, result[8].KiroCredits)
	})

	t.Run("single fallback", func(t *testing.T) {
		svc := &AccountUsageService{usageLogRepo: &usageLogWindowBatchRepoStub{
			batchErr:     context.DeadlineExceeded,
			singleResult: stats,
		}}
		result, err := svc.GetTodayStatsBatch(context.Background(), []int64{7})
		require.NoError(t, err)
		require.InDelta(t, 0.17, result[7].KiroCredits, 1e-9)
	})
}
