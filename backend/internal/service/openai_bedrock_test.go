package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestBedrockOpenAIAccountIdentityAndBaseURL(t *testing.T) {
	tests := []struct {
		name       string
		account    *Account
		wantMarker bool
		wantURL    string
	}{
		{
			name: "default region",
			account: &Account{
				Platform:    PlatformOpenAI,
				Type:        AccountTypeAPIKey,
				Credentials: map[string]any{"bedrock": true},
			},
			wantMarker: true,
			wantURL:    "https://bedrock-mantle.us-east-1.api.aws/openai/v1",
		},
		{
			name: "custom region",
			account: &Account{
				Platform:    PlatformOpenAI,
				Type:        AccountTypeAPIKey,
				Credentials: map[string]any{"bedrock": "true", "aws_region": "eu-west-1"},
			},
			wantMarker: true,
			wantURL:    "https://bedrock-mantle.eu-west-1.api.aws/openai/v1",
		},
		{
			name: "explicit base URL wins",
			account: &Account{
				Platform: PlatformOpenAI,
				Type:     AccountTypeAPIKey,
				Credentials: map[string]any{
					"bedrock":    true,
					"aws_region": "us-west-2",
					"base_url":   "https://proxy.example/openai/v1",
				},
			},
			wantMarker: true,
			wantURL:    "https://proxy.example/openai/v1",
		},
		{
			name: "false marker",
			account: &Account{
				Platform:    PlatformOpenAI,
				Type:        AccountTypeAPIKey,
				Credentials: map[string]any{"bedrock": "false"},
			},
			wantMarker: false,
			wantURL:    "https://api.openai.com",
		},
		{
			name: "OAuth account cannot be Bedrock API key",
			account: &Account{
				Platform:    PlatformOpenAI,
				Type:        AccountTypeOAuth,
				Credentials: map[string]any{"bedrock": true},
			},
			wantMarker: false,
		},
		{
			name: "other platform cannot be OpenAI Bedrock",
			account: &Account{
				Platform:    PlatformAnthropic,
				Type:        AccountTypeAPIKey,
				Credentials: map[string]any{"bedrock": true},
			},
			wantMarker: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.wantMarker, tt.account.IsOpenAIBedrock())
			if tt.wantURL != "" {
				require.Equal(t, tt.wantURL, tt.account.GetOpenAIBaseURL())
			}
		})
	}
}

func TestBuildOpenAIBedrockBaseURL(t *testing.T) {
	require.Equal(t, "https://bedrock-mantle.us-east-1.api.aws/openai/v1", BuildOpenAIBedrockBaseURL(""))
	require.Equal(t, "https://bedrock-mantle.ap-southeast-1.api.aws/openai/v1", BuildOpenAIBedrockBaseURL(" ap-southeast-1 "))
}

func TestBedrockOpenAIModelMappingAndPrefixing(t *testing.T) {
	account := &Account{
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"bedrock": true},
	}

	require.Equal(t, "openai.gpt-6.1-sol", account.GetMappedModel("gpt-6.1-sol"))
	require.Equal(t, "gpt-5.5", account.GetMappedModel("gpt-5.5"))

	tests := []struct {
		model string
		want  string
	}{
		{model: "gpt-6.1-sol", want: "openai.gpt-6.1-sol"},
		{model: "gpt-5.5", want: "openai.gpt-5.5"},
		{model: "openai.gpt-5.5", want: "openai.gpt-5.5"},
		{model: "openai/gpt-5.5", want: "openai.gpt-5.5"},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			require.Equal(t, tt.want, normalizeOpenAIModelForUpstream(account, tt.model))
		})
	}
}

func TestBedrockOpenAIModelSupport(t *testing.T) {
	defaultAccount := &Account{
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"bedrock": true},
	}
	mappedAccount := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"bedrock":       true,
			"model_mapping": map[string]any{"my-alias": "openai.gpt-6-sol"},
		},
	}

	tests := []struct {
		name    string
		account *Account
		model   string
		want    bool
	}{
		{name: "mapped model", account: defaultAccount, model: "gpt-6.1-sol", want: true},
		{name: "gpt prefix", account: defaultAccount, model: "gpt-5.5", want: true},
		{name: "trimmed and lowercased gpt prefix", account: defaultAccount, model: "  GPT-5.5  ", want: true},
		{name: "bedrock model id", account: defaultAccount, model: "openai.gpt-5.5", want: true},
		{name: "slash alias", account: defaultAccount, model: "openai/gpt-6.1-sol", want: true},
		{name: "custom mapped alias", account: mappedAccount, model: "my-alias", want: true},
		{name: "claude model", account: defaultAccount, model: "claude-sonnet-5-5", want: false},
		{name: "deepseek model", account: defaultAccount, model: "deepseek-v4", want: false},
		{name: "non-gpt slash alias", account: defaultAccount, model: "openai/claude-sonnet-5-5", want: false},
		{name: "empty model", account: defaultAccount, model: "", want: false},
		{name: "blank model", account: defaultAccount, model: "  ", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.account.IsModelSupported(tt.model))
		})
	}
}

func TestBedrockOpenAIRequestHeaderHygiene(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-6.1-sol","input":"hello"}`)
	codexOnlyHeaders := map[string]string{
		"Originator":          "codex_cli_rs",
		"Session_Id":          "session",
		"Session-Id":          "session-hyphen",
		"Conversation_Id":     "conversation",
		"Thread-Id":           "thread",
		"X-Client-Request-Id": "request",
		"X-Codex-Window-ID":   "window",
		"X-Codex-Turn-State":  "ticket",
		"ChatGPT-Account-Id":  "account",
	}
	builders := map[string]func(*OpenAIGatewayService, *gin.Context, *Account) (*http.Request, error){
		"responses": func(svc *OpenAIGatewayService, c *gin.Context, account *Account) (*http.Request, error) {
			return svc.buildUpstreamRequest(context.Background(), c, account, body, "bedrock-key", false, "", false)
		},
		"passthrough": func(svc *OpenAIGatewayService, c *gin.Context, account *Account) (*http.Request, error) {
			return svc.buildUpstreamRequestOpenAIPassthrough(context.Background(), c, account, body, "bedrock-key")
		},
	}

	for name, build := range builders {
		t.Run(name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
			for key, value := range codexOnlyHeaders {
				c.Request.Header.Set(key, value)
			}

			svc := &OpenAIGatewayService{cfg: &config.Config{
				Security: config.SecurityConfig{
					URLAllowlist: config.URLAllowlistConfig{Enabled: false},
				},
			}}
			account := &Account{
				Platform:    PlatformOpenAI,
				Type:        AccountTypeAPIKey,
				Credentials: map[string]any{"bedrock": true, "api_key": "bedrock-key"},
			}

			req, err := build(svc, c, account)
			require.NoError(t, err)
			defer func() { _ = req.Body.Close() }()

			require.Equal(t, "https://bedrock-mantle.us-east-1.api.aws/openai/v1/responses", req.URL.String())
			require.Equal(t, "Bearer bedrock-key", req.Header.Get("Authorization"))
			require.Equal(t, "application/json", req.Header.Get("Content-Type"))
			for key := range codexOnlyHeaders {
				require.Empty(t, req.Header.Get(key), "Codex-only header %s must not reach Bedrock", key)
			}
		})
	}
}
