package setup

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/auth"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/models"
)

func TestPrintDeviceBannerQuiet(t *testing.T) {
	dc := &auth.DeviceCode{
		UserCode:        "TEST-1234",
		VerificationURI: "https://login.microsoft.com/device",
		Interval:        5,
	}
	// Quiet banner should not panic or produce errors
	printDeviceBanner(true, dc)
}

func TestPrintPKCEBannerQuiet(t *testing.T) {
	printPKCEBanner(true, "https://login.microsoftonline.com/authorize")
}

func TestPrintNativeSuccess(t *testing.T) {
	res := &auth.TokenResult{
		TenantID: "tenant-abc",
		UserOID:  "oid-def",
		Email:    "test@example.com",
	}
	// Should not panic
	printNativeSuccess(res)
}

func TestSaveCredentialStores(t *testing.T) {
	tmpDir := t.TempDir()
	refreshFile := filepath.Join(tmpDir, "rt_90day.txt")
	cacheFile := filepath.Join(tmpDir, "cache.json")
	envFile := filepath.Join(tmpDir, ".env")

	res := &auth.TokenResult{
		AccessToken:  "mock-access-token",
		RefreshToken: "mock-refresh-token",
		TenantID:     "tenant-test-id",
		UserOID:      "user-test-oid",
		ExpiresAt:    time.Now().Add(time.Hour),
	}

	if err := saveCredentialStoresTo(res, models.DefaultDeviceClientID, refreshFile, cacheFile, envFile); err != nil {
		t.Fatalf("saveCredentialStoresTo failed: %v", err)
	}

	// Verify rt_90day.txt exists and is non-empty
	if data, err := os.ReadFile(refreshFile); err != nil || len(data) == 0 {
		t.Errorf("expected refresh token file to be written, err=%v len=%d", err, len(data))
	}

	// Verify cache.json exists
	if data, err := os.ReadFile(cacheFile); err != nil || len(data) == 0 {
		t.Errorf("expected cache file to be written, err=%v len=%d", err, len(data))
	}

	// Verify .env exists and contains the client ID
	if data, err := os.ReadFile(envFile); err != nil {
		t.Errorf("expected .env file to be written, err=%v", err)
	} else if len(data) == 0 {
		t.Error("expected .env file to be non-empty")
	}
}
