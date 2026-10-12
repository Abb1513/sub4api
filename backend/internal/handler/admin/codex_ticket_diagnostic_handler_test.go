package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/server/middleware"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// fakeCodexDiagnosticProbe 只记录调用并按模型返回结果，用来把 handler 自身的
// 行为（模型校验、挑战抽签、结果映射、取消）与真实上游隔离开。
type fakeCodexDiagnosticProbe struct {
	calls      int
	accountID  int64
	timeout    time.Duration
	challenges map[string]service.ModelTraceChallenge
	outputs    map[string]string
	reasons    map[string]string
	statuses   map[string]int
	onCall     func(model string)
}

func (f *fakeCodexDiagnosticProbe) RunCodexNativeDiagnostic(_ context.Context, accountID int64, model string, challenge service.ModelTraceChallenge, timeout time.Duration) service.CodexNativeDiagnostic {
	f.calls++
	f.accountID = accountID
	f.timeout = timeout
	if f.challenges == nil {
		f.challenges = make(map[string]service.ModelTraceChallenge, 1)
	}
	f.challenges[model] = challenge
	result := service.CodexNativeDiagnostic{
		Output:     f.outputs[model],
		HTTPStatus: f.statuses[model],
		Reason:     f.reasons[model],
	}
	if f.onCall != nil {
		f.onCall(model)
	}
	return result
}

func newCodexDiagnosticHandler(t *testing.T, models []string, probe codexDiagnosticProbe) (*AccountHandler, *gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"models": models})
	require.NoError(t, err)
	writer := httptest.NewRecorder()
	client, _ := gin.CreateTestContext(writer)
	client.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/41/codex-ticket-diagnostic", strings.NewReader(string(payload)))
	client.Request.Header.Set("Content-Type", "application/json")
	client.Params = gin.Params{{Key: "id", Value: "41"}}
	client.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 17})
	return &AccountHandler{codexDiagnostic: probe}, client, writer
}

func TestCodexDiagnosticRunsAccountNativeProbePerModel(t *testing.T) {
	probe := &fakeCodexDiagnosticProbe{
		outputs: map[string]string{
			"gpt-5.4": strings.Repeat("1 ", 170),
			"gpt-5.5": strings.Repeat("1 ", 170),
		},
		statuses: map[string]int{"gpt-5.4": http.StatusOK, "gpt-5.5": http.StatusOK},
	}
	handler, client, writer := newCodexDiagnosticHandler(t, []string{"gpt-5.4", "gpt-5.5"}, probe)
	challenges := []service.ModelTraceChallenge{
		{Prompt: "first challenge: 292 个 1 到 355 的整数", ExpectedCount: 292},
		{Prompt: "second challenge: 332 个 1 到 355 的整数", ExpectedCount: 332},
	}
	generated := 0
	handler.diagnoseCodexModels(client, func() (service.ModelTraceChallenge, error) {
		require.Less(t, generated, len(challenges))
		challenge := challenges[generated]
		generated++
		return challenge, nil
	})

	require.Equal(t, http.StatusOK, writer.Code)
	require.Equal(t, 2, generated)
	require.Equal(t, 2, probe.calls)
	require.EqualValues(t, 41, probe.accountID)
	require.Equal(t, 120*time.Second, probe.timeout)
	// 请求与判定必须共用同一次抽签：第 i 个模型拿到第 i 个挑战。
	require.Equal(t, challenges[0], probe.challenges["gpt-5.4"])
	require.Equal(t, challenges[1], probe.challenges["gpt-5.5"])

	items := gjson.GetBytes(writer.Body.Bytes(), "data.items").Array()
	require.Len(t, items, 2)
	require.NotEqual(t, "insufficient_numbers", items[0].Get("reason").String())
	require.NotEmpty(t, items[0].Get("predicted_model").String())
	require.Equal(t, "insufficient_numbers", items[1].Get("reason").String())
	require.Equal(t, "failed", items[1].Get("status").String())
	for _, item := range items {
		require.EqualValues(t, 170, item.Get("parsed_number_count").Int())
	}
}

func TestCodexDiagnosticChallengeGenerationFailureDoesNotSendRequest(t *testing.T) {
	probe := &fakeCodexDiagnosticProbe{}
	handler, client, writer := newCodexDiagnosticHandler(t, []string{"gpt-5.4"}, probe)
	generated := 0
	handler.diagnoseCodexModels(client, func() (service.ModelTraceChallenge, error) {
		generated++
		return service.ModelTraceChallenge{}, errors.New("entropy unavailable")
	})
	require.Equal(t, http.StatusOK, writer.Code)
	require.Equal(t, 1, generated)
	require.Zero(t, probe.calls)
	item := gjson.GetBytes(writer.Body.Bytes(), "data.items.0")
	require.Equal(t, "failed", item.Get("status").String())
	require.Equal(t, "challenge_generation_failed", item.Get("reason").String())
	require.False(t, item.Get("http_status").Exists())
}

func TestCodexDiagnosticDefaultUsesModelTraceChallenge(t *testing.T) {
	probe := &fakeCodexDiagnosticProbe{outputs: map[string]string{"gpt-5.4": "1"}}
	handler, client, writer := newCodexDiagnosticHandler(t, []string{"gpt-5.4"}, probe)
	handler.DiagnoseCodexModels(client)
	require.Equal(t, http.StatusOK, writer.Code)
	require.Equal(t, 1, probe.calls)
	match := regexp.MustCompile(` ([0-9]+) 个 1 到 355`).FindStringSubmatch(probe.challenges["gpt-5.4"].Prompt)
	require.Len(t, match, 2)
	count, err := strconv.Atoi(match[1])
	require.NoError(t, err)
	require.GreaterOrEqual(t, count, 292)
	require.LessOrEqual(t, count, 332)
	require.Equal(t, "insufficient_numbers", gjson.GetBytes(writer.Body.Bytes(), "data.items.0.reason").String())
}

func TestCodexDiagnosticValidatesModels(t *testing.T) {
	for _, tc := range []struct {
		name   string
		models []string
		status int
	}{
		{"unknown model rejected", []string{"gpt-7-nope"}, http.StatusBadRequest},
		{"duplicate rejected", []string{"gpt-5.4", "gpt-5.4"}, http.StatusBadRequest},
		{"fingerprint-less catalog model accepted", []string{"gpt-6.1-sol"}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := &fakeCodexDiagnosticProbe{outputs: map[string]string{"gpt-6.1-sol": "1"}}
			handler, client, writer := newCodexDiagnosticHandler(t, tc.models, probe)
			handler.DiagnoseCodexModels(client)
			require.Equal(t, tc.status, writer.Code)
			if tc.status == http.StatusBadRequest {
				require.Zero(t, probe.calls, "invalid models must not reach the upstream probe")
			} else {
				require.Equal(t, 1, probe.calls)
			}
		})
	}
}

func TestCodexDiagnosticProbeFailureKeepsStatusFailed(t *testing.T) {
	probe := &fakeCodexDiagnosticProbe{
		reasons:  map[string]string{"gpt-6-astra": "credentials"},
		statuses: map[string]int{"gpt-6-astra": http.StatusServiceUnavailable},
	}
	handler, client, writer := newCodexDiagnosticHandler(t, []string{"gpt-6-astra"}, probe)
	handler.DiagnoseCodexModels(client)
	require.Equal(t, http.StatusOK, writer.Code)
	item := gjson.GetBytes(writer.Body.Bytes(), "data.items.0")
	require.Equal(t, "failed", item.Get("status").String())
	require.Equal(t, "credentials", item.Get("reason").String())
	require.EqualValues(t, http.StatusServiceUnavailable, item.Get("http_status").Int())
	require.False(t, item.Get("predicted_model").Exists())
}

func TestCodexDiagnosticCancellationStopsFollowingModels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := &fakeCodexDiagnosticProbe{}
	probe.onCall = func(string) { cancel() }
	handler, client, writer := newCodexDiagnosticHandler(t, []string{"gpt-6-astra", "gpt-5.4"}, probe)
	client.Request = client.Request.WithContext(ctx)
	handler.DiagnoseCodexModels(client)
	require.Equal(t, 1, probe.calls)
	require.True(t, gjson.GetBytes(writer.Body.Bytes(), "data.canceled").Bool())
	require.Len(t, gjson.GetBytes(writer.Body.Bytes(), "data.items").Array(), 1)
}

func TestCodexDiagnosticCanceledBeforeFirstModelDoesNotSend(t *testing.T) {
	probe := &fakeCodexDiagnosticProbe{}
	handler, client, writer := newCodexDiagnosticHandler(t, []string{"gpt-5.4"}, probe)
	ctx, cancel := context.WithCancel(client.Request.Context())
	cancel()
	client.Request = client.Request.WithContext(ctx)
	handler.DiagnoseCodexModels(client)
	require.Zero(t, probe.calls)
	require.True(t, gjson.GetBytes(writer.Body.Bytes(), "data.canceled").Bool())
	require.Empty(t, gjson.GetBytes(writer.Body.Bytes(), "data.items").Array())
}

func TestCodexDiagnosticUnavailableWithoutProbe(t *testing.T) {
	handler, client, writer := newCodexDiagnosticHandler(t, []string{"gpt-5.4"}, nil)
	handler.DiagnoseCodexModels(client)
	require.Equal(t, http.StatusUnprocessableEntity, writer.Code)
	require.Contains(t, writer.Body.String(), "CODEX_TICKET_UNAVAILABLE")
}
