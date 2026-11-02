//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/kirocooldown"
	"github.com/MACOS-DO/sub4api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestBuildKiroWebSearchMCPRequest_UsesUnderscoredMetaKeys(t *testing.T) {
	req := buildKiroWebSearchMCPRequest("golang concurrency")

	body, err := json.Marshal(req)
	require.NoError(t, err)

	require.Equal(t, "tools/call", gjson.GetBytes(body, "method").String())
	require.Equal(t, "web_search", gjson.GetBytes(body, "params.name").String())
	require.Equal(t, "golang concurrency", gjson.GetBytes(body, "params.arguments.query").String())
	require.True(t, gjson.GetBytes(body, "params.arguments._meta._isValid").Bool())
	require.Equal(t, "query", gjson.GetBytes(body, "params.arguments._meta._activePath.0").String())
	require.Equal(t, "query", gjson.GetBytes(body, "params.arguments._meta._completedPaths.0.0").String())
	require.False(t, gjson.GetBytes(body, "params.arguments._meta.isValid").Exists())
	require.False(t, gjson.GetBytes(body, "params.arguments._meta.activePath").Exists())
	require.False(t, gjson.GetBytes(body, "params.arguments._meta.completedPaths").Exists())
}

func TestWriteAnthropicMessageStart_UsesCacheEmulationUsage(t *testing.T) {
	var out bytes.Buffer
	err := writeAnthropicMessageStart(&out, "msg_test", "claude-sonnet-4-6", 100, &kiroCacheEmulationUsage{
		InputTokens:              25,
		CacheCreationInputTokens: 75,
		CacheReadInputTokens:     0,
	})
	require.NoError(t, err)
	body := out.String()
	require.Contains(t, body, `"input_tokens":25`)
	require.Contains(t, body, `"cache_creation_input_tokens":75`)
	require.Contains(t, body, `"cache_read_input_tokens":0`)
}

func TestExecuteKiroWebSearch_ReturnsMCPResultsWithoutGeneration(t *testing.T) {
	upstream := &kiroWebSearchHTTPUpstream{body: `{"jsonrpc":"2.0","result":{"content":[{"type":"text","text":"{\"results\":[{\"title\":\"Shanghai Weather\",\"url\":\"https://example.com/weather\",\"snippet\":\"Cloudy\"}]}"}]}}`}
	svc := &GatewayService{
		httpUpstream:        upstream,
		tlsFPProfileService: &TLSFingerprintProfileService{},
		kiroCooldownStore:   allowKiroCooldownStore{},
	}
	account := &Account{
		ID:       7,
		Platform: PlatformKiro,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"api_region": "us-east-1",
		},
	}
	body := []byte(`{"model":"claude-sonnet","messages":[{"role":"user","content":"shanghai weather"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`)

	result, err := svc.executeKiroWebSearch(context.Background(), account, nil, nil, body, "claude-sonnet", "claude-sonnet", "token", nil)

	require.NoError(t, err)
	require.Len(t, upstream.paths, 1)
	require.Equal(t, "/mcp", upstream.paths[0])
	require.NotContains(t, strings.Join(upstream.paths, ","), "generateAssistantResponse")
	require.Equal(t, "end_turn", gjson.GetBytes(result.ResponseBody, "stop_reason").String())
	require.Contains(t, gjson.GetBytes(result.ResponseBody, "content.0.text").String(), `I'll search for "shanghai weather".`)
	require.Equal(t, "web_search", gjson.GetBytes(result.ResponseBody, "content.1.name").String())
	require.Equal(t, "https://example.com/weather", gjson.GetBytes(result.ResponseBody, "content.2.content.0.url").String())
	require.Contains(t, gjson.GetBytes(result.ResponseBody, "content.3.text").String(), "Shanghai Weather")
	require.Equal(t, result.Usage.OutputTokens, int(gjson.GetBytes(result.ResponseBody, "usage.output_tokens").Int()))
	require.Positive(t, result.Usage.OutputTokens)
}

func TestStreamKiroWebSearch_ReturnsMCPResultsWithoutGeneration(t *testing.T) {
	upstream := &kiroWebSearchHTTPUpstream{body: `{"jsonrpc":"2.0","result":{"content":[{"type":"text","text":"{\"results\":[{\"title\":\"Shanghai Weather\",\"url\":\"https://example.com/weather\",\"snippet\":\"Cloudy\"}]}"}]}}`}
	svc := &GatewayService{
		httpUpstream:        upstream,
		tlsFPProfileService: &TLSFingerprintProfileService{},
		kiroCooldownStore:   allowKiroCooldownStore{},
	}
	account := &Account{
		ID:       7,
		Platform: PlatformKiro,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"api_region": "us-east-1",
		},
	}
	body := []byte(`{"model":"claude-sonnet","stream":true,"messages":[{"role":"user","content":"shanghai weather"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`)
	var output bytes.Buffer

	err := svc.streamKiroWebSearchAsAnthropic(context.Background(), account, body, "claude-sonnet", "claude-sonnet", "token", 11, nil, &output, nil)

	require.NoError(t, err)
	require.Equal(t, []string{"/mcp"}, upstream.paths)
	stream := output.String()
	require.Contains(t, stream, "event: message_start")
	require.Contains(t, stream, `"name":"web_search"`)
	require.Contains(t, stream, "https://example.com/weather")
	require.Contains(t, stream, "Shanghai Weather")
	require.Contains(t, stream, `"stop_reason":"end_turn"`)
	require.Contains(t, stream, "event: message_stop")
	require.NotContains(t, stream, `"type":"tool_use"`)
	require.Equal(t, 1, strings.Count(stream, "event: message_start"))
}

type kiroWebSearchHTTPUpstream struct {
	body  string
	paths []string
}

func (u *kiroWebSearchHTTPUpstream) Do(*http.Request, string, int64, int) (*http.Response, error) {
	return nil, errors.New("unexpected Do call")
}

func (u *kiroWebSearchHTTPUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	u.paths = append(u.paths, req.URL.Path)
	if strings.Contains(req.URL.Path, "generateAssistantResponse") {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"message":"ValidationException"}`)),
		}, nil
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(u.body)),
	}, nil
}

type allowKiroCooldownStore struct{}

func (allowKiroCooldownStore) CheckCooldown(context.Context, string) error { return nil }
func (allowKiroCooldownStore) MarkSuccess(context.Context, string) error   { return nil }
func (allowKiroCooldownStore) Mark429(context.Context, string) (time.Duration, error) {
	return 0, nil
}
func (allowKiroCooldownStore) MarkSuspended(context.Context, string) (time.Duration, error) {
	return 0, nil
}
func (allowKiroCooldownStore) GetState(context.Context, string) (*kirocooldown.State, error) {
	return &kirocooldown.State{}, nil
}
func (allowKiroCooldownStore) ClearEarliestTransientCooldown(context.Context, []string) (bool, error) {
	return false, nil
}
