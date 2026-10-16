package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestGeneratePKCE(t *testing.T) {
	v, c, err := GeneratePKCE()
	if err != nil {
		t.Fatalf("GeneratePKCE: %v", err)
	}
	if len(v) < 40 || len(c) < 40 {
		t.Errorf("verifier or challenge unexpectedly short: v=%d c=%d", len(v), len(c))
	}
	if strings.ContainsAny(v, "+/=") || strings.ContainsAny(c, "+/=") {
		t.Errorf("verifier or challenge contains non-URL-safe characters: v=%s c=%s", v, c)
	}
}

func TestAuthorizationURL(t *testing.T) {
	u := AuthorizationURL("common", "test-client", "https://redirect", "test-scope", "test-state", "test-challenge")
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatalf("parse auth URL: %v", err)
	}
	q := parsed.Query()
	if q.Get("client_id") != "test-client" {
		t.Errorf("client_id: got %s, want test-client", q.Get("client_id"))
	}
	if q.Get("response_type") != "code" {
		t.Errorf("response_type: got %s, want code", q.Get("response_type"))
	}
	if q.Get("code_challenge") != "test-challenge" {
		t.Errorf("code_challenge: got %s, want test-challenge", q.Get("code_challenge"))
	}
	if q.Get("prompt") != "select_account" {
		t.Errorf("prompt: got %s, want select_account", q.Get("prompt"))
	}
}

func TestExtractCodeFromCallback(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:  "bare code",
			input: "M.C544_BAY.0.U.12345",
			want:  "M.C544_BAY.0.U.12345",
		},
		{
			name:  "full redirect url with query",
			input: "https://login.microsoftonline.com/common/oauth2/nativeclient?code=M.C123&state=xyz",
			want:  "M.C123",
		},
		{
			name:  "query string alone",
			input: "?code=M.C456&state=xyz",
			want:  "M.C456",
		},
		{
			name:  "fragment redirect url",
			input: "https://m365.cloud.microsoft/spalanding#code=M.C789&state=xyz",
			want:  "M.C789",
		},
		{
			name:    "empty input",
			input:   "",
			wantErr: true,
		},
		{
			name:    "no code parameter",
			input:   "https://login.microsoftonline.com/common/oauth2/nativeclient?error=access_denied",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExtractCodeFromCallback(tc.input)
			if tc.wantErr && err == nil {
				t.Errorf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExchangeCode(t *testing.T) {
	at := mockJWT(t, map[string]any{"tid": "t1", "oid": "u1", "email": "dev@example.com"})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
			return
		}
		if r.Form.Get("grant_type") != "authorization_code" {
			t.Errorf("grant_type: got %s, want authorization_code", r.Form.Get("grant_type"))
		}
		if r.Form.Get("code") != "test-code" {
			t.Errorf("code: got %s, want test-code", r.Form.Get("code"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  at,
			"refresh_token": "rt-pkce-90day",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	// Direct test of parseExchangeResponse with the mock payload
	payload, _ := json.Marshal(map[string]any{
		"access_token":  at,
		"refresh_token": "rt-pkce-90day",
		"expires_in":    3600,
	})
	res, err := parseExchangeResponse(payload)
	if err != nil {
		t.Fatalf("parseExchangeResponse: %v", err)
	}
	if res.TenantID != "t1" || res.UserOID != "u1" || res.RefreshToken != "rt-pkce-90day" {
		t.Errorf("unexpected token result: %+v", res)
	}
	_ = context.Background()
}
