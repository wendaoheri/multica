package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func boolPtr(b bool) *bool { return &b }

func TestGoogleTokenErrorDetail(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		wantCode      string
		wantDescSub   string // substring the description must contain
		wantDescEmpty bool   // description must be exactly ""
		wantEmpty     bool   // both fields must be empty
	}{
		{
			name:        "structured_error_fields_extracted",
			body:        `{"error":"invalid_grant","error_description":"Code was already redeemed."}`,
			wantCode:    "invalid_grant",
			wantDescSub: "Code was already redeemed.",
		},
		{
			name:          "error_only",
			body:          `{"error":"invalid_request"}`,
			wantCode:      "invalid_request",
			wantDescEmpty: true,
		},
		{
			name:      "malformed_json_yields_empty",
			body:      `{"error":"invalid_grant",`,
			wantEmpty: true,
		},
		{
			name:      "empty_body_yields_empty",
			body:      "",
			wantEmpty: true,
		},
		{
			name:      "html_error_page_yields_empty",
			body:      "<html><body>502 Bad Gateway</body></html>",
			wantEmpty: true,
		},
		{
			name:      "json_without_error_fields_yields_empty",
			body:      `{"access_token":"ya29.secret","token_type":"Bearer"}`,
			wantEmpty: true,
		},
		{
			name:        "whitespace_trimmed",
			body:        `{"error":" invalid_grant ","error_description":"  expired  "}`,
			wantCode:    "invalid_grant",
			wantDescSub: "expired",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, desc := googleTokenErrorDetail([]byte(tt.body))
			if tt.wantEmpty {
				if code != "" || desc != "" {
					t.Fatalf("expected empty fields, got code=%q desc=%q", code, desc)
				}
				return
			}
			if code != tt.wantCode {
				t.Fatalf("code = %q, want %q", code, tt.wantCode)
			}
			if tt.wantDescEmpty && desc != "" {
				t.Fatalf("desc = %q, want empty", desc)
			}
			if tt.wantDescSub != "" && !strings.Contains(desc, tt.wantDescSub) {
				t.Fatalf("desc = %q, want substring %q", desc, tt.wantDescSub)
			}
		})
	}
}

func TestGoogleTokenErrorDetailNeverLeaksRawBody(t *testing.T) {
	// A body carrying credentials must not surface them through either field.
	body := `{"error":"invalid_grant","error_description":"check Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxIn0.sig and API_KEY=super-secret-value"}`
	code, desc := googleTokenErrorDetail([]byte(body))
	if code != "invalid_grant" {
		t.Fatalf("code = %q, want invalid_grant", code)
	}
	if strings.Contains(desc, "eyJ") || strings.Contains(desc, "super-secret-value") {
		t.Fatalf("description leaks raw body content: %q", desc)
	}
}

func TestGoogleTokenErrorDetailTruncatesLongDescription(t *testing.T) {
	long := strings.Repeat("日", googleTokenErrorDescriptionMaxRunes+50)
	body := `{"error":"invalid_grant","error_description":"` + long + `"}`
	_, desc := googleTokenErrorDetail([]byte(body))
	if got := len([]rune(desc)); got != googleTokenErrorDescriptionMaxRunes {
		t.Fatalf("description rune length = %d, want %d", got, googleTokenErrorDescriptionMaxRunes)
	}
}

func TestGoogleUserInfoEmailVerified(t *testing.T) {
	tests := []struct {
		name string
		user googleUserInfo
		want bool
	}{
		{"verified_email_true", googleUserInfo{VerifiedEmail: boolPtr(true)}, true},
		{"email_verified_true", googleUserInfo{EmailVerified: boolPtr(true)}, true},
		{"both_true", googleUserInfo{VerifiedEmail: boolPtr(true), EmailVerified: boolPtr(true)}, true},
		{"verified_email_false", googleUserInfo{VerifiedEmail: boolPtr(false)}, false},
		{"email_verified_false", googleUserInfo{EmailVerified: boolPtr(false)}, false},
		{"both_false", googleUserInfo{VerifiedEmail: boolPtr(false), EmailVerified: boolPtr(false)}, false},
		{"missing_flags_fail_closed", googleUserInfo{Email: "a@x.com"}, false},
		{"one_true_one_false", googleUserInfo{VerifiedEmail: boolPtr(false), EmailVerified: boolPtr(true)}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.user.isEmailVerified(); got != tt.want {
				t.Fatalf("isEmailVerified() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsDevVerificationCodeDoubleGating(t *testing.T) {
	tests := []struct {
		name    string
		appEnv  string
		enabled string
		devCode string
		code    string
		want    bool
	}{
		{"both_gates_open_match", "development", "true", "888888", "888888", true},
		{"app_env_unset_counts_non_production", "", "true", "888888", "888888", true},
		{"enabled_flag_numeric_one", "development", "1", "888888", "888888", true},
		{"enabled_flag_case_and_space_insensitive", "development", " TRUE ", "888888", "888888", true},
		{"dev_code_surrounding_whitespace", "development", "true", " 888888 ", "888888", true},
		{"production_blocks_even_if_enabled", "production", "true", "888888", "888888", false},
		{"production_case_insensitive", "Production", "true", "888888", "888888", false},
		{"enabled_flag_missing", "development", "", "888888", "888888", false},
		{"enabled_flag_false", "development", "false", "888888", "888888", false},
		{"enabled_flag_zero", "development", "0", "888888", "888888", false},
		{"enabled_flag_garbage", "development", "yes-please", "888888", "888888", false},
		{"dev_code_unset", "development", "true", "", "888888", false},
		{"dev_code_wrong", "development", "true", "888888", "123456", false},
		{"dev_code_not_six_digits_short", "development", "true", "12345", "12345", false},
		{"dev_code_not_six_digits_long", "development", "true", "1234567", "1234567", false},
		{"dev_code_non_numeric", "development", "true", "abcdef", "abcdef", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("APP_ENV", tt.appEnv)
			t.Setenv(devVerificationCodeEnabledEnv, tt.enabled)
			t.Setenv(devVerificationCodeEnv, tt.devCode)

			if got := isDevVerificationCode(tt.code); got != tt.want {
				t.Fatalf("isDevVerificationCode(%q) = %v, want %v (APP_ENV=%q enabled=%q devCode=%q)",
					tt.code, got, tt.want, tt.appEnv, tt.enabled, tt.devCode)
			}
		})
	}
}

// TestVerifyCodeDevBypassMalformedEnableFlag covers the handler-path gaps the
// core dev-code tests in handler_test.go do not: an explicitly falsy enable
// flag, and a wrong 6-digit code presented while both gates are open.
func TestVerifyCodeDevBypassMalformedEnableFlag(t *testing.T) {
	const email = "dev-bypass-gating@multica.ai"
	const devCode = "888888"

	seedCode := func(t *testing.T) {
		t.Helper()
		// The dev bypass only skips the comparison — an unused, unexpired
		// code row for the email must still exist. A fresh row per attempt
		// also sidesteps the used=FALSE/attempts<5 filters after prior
		// subtests consumed or failed on earlier rows.
		_, err := testHandler.Queries.CreateVerificationCode(context.Background(), db.CreateVerificationCodeParams{
			Email:     email,
			Code:      "999999",
			ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(10 * time.Minute), Valid: true},
		})
		if err != nil {
			t.Fatalf("failed to seed verification code: %v", err)
		}
	}
	t.Cleanup(func() {
		if testPool != nil {
			_, _ = testPool.Exec(context.Background(), `DELETE FROM verification_code WHERE email = $1`, email)
			_, _ = testPool.Exec(context.Background(), `DELETE FROM "user" WHERE email = $1`, email)
		}
	})

	verify := func(t *testing.T, code string) int {
		t.Helper()
		body, err := json.Marshal(VerifyCodeRequest{Email: email, Code: code})
		if err != nil {
			t.Fatalf("failed to marshal request: %v", err)
		}
		rec := httptest.NewRecorder()
		testHandler.VerifyCode(rec, httptest.NewRequest(http.MethodPost, "/auth/verify-code", bytes.NewReader(body)))
		return rec.Code
	}

	t.Run("enabled_flag_false_rejects", func(t *testing.T) {
		t.Setenv("APP_ENV", "development")
		t.Setenv(devVerificationCodeEnabledEnv, "false")
		t.Setenv(devVerificationCodeEnv, devCode)
		seedCode(t)

		if got := verify(t, devCode); got != http.StatusBadRequest {
			t.Fatalf("VerifyCode with falsy enable flag = %d, want %d", got, http.StatusBadRequest)
		}
	})

	// With both gates open, a code that is neither the dev code nor the DB
	// code must still be rejected.
	t.Run("wrong_code_still_rejected_when_gates_open", func(t *testing.T) {
		t.Setenv("APP_ENV", "development")
		t.Setenv(devVerificationCodeEnabledEnv, "true")
		t.Setenv(devVerificationCodeEnv, devCode)
		seedCode(t)

		if got := verify(t, "000000"); got != http.StatusBadRequest {
			t.Fatalf("VerifyCode with wrong code = %d, want %d", got, http.StatusBadRequest)
		}
	})
}

func TestIsProductionEnv(t *testing.T) {
	tests := []struct {
		name   string
		appEnv string
		want   bool
	}{
		{"production", "production", true},
		{"production_mixed_case_with_space", " Production ", true},
		{"staging", "staging", false},
		{"unset", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("APP_ENV", tt.appEnv)
			if got := isProductionEnv(); got != tt.want {
				t.Fatalf("isProductionEnv() = %v, want %v", got, tt.want)
			}
		})
	}
}
