package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAPIKeyAuthSnapshotKiroGroupFieldsRoundtrip(t *testing.T) {
	svc := &APIKeyService{}
	groupID := int64(77)
	apiKey := &APIKey{
		ID:      91,
		UserID:  41,
		GroupID: &groupID,
		Status:  StatusActive,
		User:    &User{ID: 41, Status: StatusActive},
		Group: &Group{
			ID:                              groupID,
			Platform:                        PlatformKiro,
			Status:                          StatusActive,
			Hydrated:                        true,
			KiroCacheEmulationEnabled:       true,
			KiroAutoStickyEnabled:           true,
			KiroStickySessionTTLSeconds:     1800,
			KiroCacheEmulationRatio:         0.95,
			KiroCacheEmulationMode:          KiroCacheEmulationModeIndependent,
			KiroCacheCreationEmulationRatio: 0.8,
			KiroCacheReadEmulationRatio:     0.9,
			KiroEndpointMode:                KiroEndpointModeKRS,
		},
	}

	snapshot := svc.snapshotFromAPIKey(context.Background(), apiKey)
	payload, err := json.Marshal(&APIKeyAuthCacheEntry{Snapshot: snapshot})
	require.NoError(t, err)
	var restored APIKeyAuthCacheEntry
	require.NoError(t, json.Unmarshal(payload, &restored))

	materialized, used, err := svc.applyAuthCacheEntry(apiKey.Key, &restored)
	require.NoError(t, err)
	require.True(t, used)
	g := materialized.Group
	require.NotNil(t, g)
	require.True(t, g.KiroCacheEmulationEnabled)
	require.True(t, g.KiroAutoStickyEnabled)
	require.Equal(t, 1800, g.KiroStickySessionTTLSeconds)
	require.InDelta(t, 0.95, g.KiroCacheEmulationRatio, 1e-12)
	require.Equal(t, KiroCacheEmulationModeIndependent, g.KiroCacheEmulationMode)
	require.InDelta(t, 0.8, g.KiroCacheCreationEmulationRatio, 1e-12)
	require.InDelta(t, 0.9, g.KiroCacheReadEmulationRatio, 1e-12)
	require.Equal(t, KiroEndpointModeKRS, g.KiroEndpointMode)
	require.True(t, g.EffectiveKiroCacheEmulationEnabled())
}
