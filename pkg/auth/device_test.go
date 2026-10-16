package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func mockJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payloadBytes, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(payloadBytes)
	return fmt.Sprintf("%s.%s.sig", header, payload)
}

func TestParseDeviceCodeResponse(t *testing.T) {
	body := []byte(`{
		"user_code": "ABCD-1234",
		"device_code": "dev-code-xyz",
		"verification_uri": "https://microsoft.com/devicelogin",
		"message": "Sign in at https://microsoft.com/devicelogin",
		"expires_in": 900,
		"interval": 5
	}`)

	dc, err := parseDeviceCodeResponse(body)
	if err != nil {
		t.Fatalf("parseDeviceCodeResponse: %v", err)
	}
	if dc.UserCode != "ABCD-1234" || dc.DeviceCode != "dev-code-xyz" {
		t.Errorf("unexpected device code: %+v", dc)
	}
	if dc.VerificationURI != "https://microsoft.com/devicelogin" {
		t.Errorf("unexpected uri: %s", dc.VerificationURI)
	}
	if dc.Interval != 5 {
		t.Errorf("expected interval 5, got %d", dc.Interval)
	}
}

func TestParseDeviceCodeResponseError(t *testing.T) {
	body := []byte(`{"error": "invalid_client", "error_description": "unknown client"}`)
	_, err := parseDeviceCodeResponse(body)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestHandleDeviceTokenBodyPending(t *testing.T) {
	pendingBody := []byte(`{"error": "authorization_pending", "error_description": "pending"}`)
	res, done, extra, err := handleDeviceTokenBody(pendingBody)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if done || res != nil || extra != 0 {
		t.Fatalf("expected pending state, got done=%t res=%v extra=%v", done, res, extra)
	}
}

func TestHandleDeviceTokenBodySuccess(t *testing.T) {
	at := mockJWT(t, map[string]any{
		"tid":                "tenant-123",
		"oid":                "user-456",
		"preferred_username": "user@example.com",
		"name":               "Test User",
	})
	successBody, _ := json.Marshal(map[string]any{
		"access_token":  at,
		"refresh_token": "rolling-rt-90day",
		"token_type":    "Bearer",
		"scope":         "sydney",
		"expires_in":    3600,
	})

	res, done, _, err := handleDeviceTokenBody(successBody)
	if err != nil {
		t.Fatalf("handleDeviceTokenBody success: %v", err)
	}
	if !done || res == nil {
		t.Fatalf("expected done=true, got done=%t res=%v", done, res)
	}
	if res.TenantID != "tenant-123" || res.UserOID != "user-456" {
		t.Errorf("unexpected claims: tid=%s oid=%s", res.TenantID, res.UserOID)
	}
	if res.Email != "user@example.com" || res.DisplayName != "Test User" {
		t.Errorf("unexpected profile: email=%s name=%s", res.Email, res.DisplayName)
	}
	if res.RefreshToken != "rolling-rt-90day" {
		t.Errorf("unexpected refresh token: %s", res.RefreshToken)
	}
}

func TestPollDeviceCodeSuccess(t *testing.T) {
	var calls atomic.Int32
	at := mockJWT(t, map[string]any{"tid": "t1", "oid": "u1", "email": "a@b.com"})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if count == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":             "authorization_pending",
				"error_description": "waiting",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  at,
			"refresh_token": "rt-new",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Use pollDeviceStep directly with the test server URL
	step1, done1, _, err1 := pollDeviceStep(ctx, srv.URL, "client", "dev-code")
	if err1 != nil || done1 || step1 != nil {
		t.Fatalf("expected pending on call 1: done=%t err=%v", done1, err1)
	}

	step2, done2, _, err2 := pollDeviceStep(ctx, srv.URL, "client", "dev-code")
	if err2 != nil || !done2 || step2 == nil {
		t.Fatalf("expected success on call 2: done=%t err=%v", done2, err2)
	}
	if step2.TenantID != "t1" || step2.UserOID != "u1" {
		t.Errorf("unexpected claims: tid=%s oid=%s", step2.TenantID, step2.UserOID)
	}
}
