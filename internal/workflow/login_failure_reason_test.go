package workflow

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mangobubu/gopay-autosms/internal/domain"
	"github.com/mangobubu/gopay-autosms/internal/gopay"
)

func TestLoginFailureReasonIncludesStageAndSanitizedUpstreamDetail(t *testing.T) {
	httpErr := &gopay.HTTPError{
		StatusCode: http.StatusForbidden,
		Body:       []byte(`{"errors":[{"code":"GoPay-112","message_title":"Akunmu diblokir sementara","message":"client_secret=TOP_SECRET_TOKEN Bearer RAW_BEARER OTP=2614 PIN=9142 account_id=590445936 phone=628123456789"}]}`),
	}
	err := fmt.Errorf("verify PIN: %w: %w", gopay.ErrLoginFailed, httpErr)

	reason := loginFailureReason(domain.ActivationStatusSettingPIN, err)
	for _, want := range []string{
		"阶段：提交新 PIN",
		"HTTP 403",
		"错误码：GoPay-112",
		"GoPay 操作失败",
		"GoPay 信息：Akunmu diblokir sementara",
		"client_secret=***",
		"Bearer ***",
		"OTP=***",
		"PIN=***",
		"account_id=***",
		"phone=***",
	} {
		if !strings.Contains(reason, want) {
			t.Fatalf("reason=%q, want it to contain %q", reason, want)
		}
	}
	for _, secret := range []string{"TOP_SECRET_TOKEN", "RAW_BEARER", "2614", "9142", "590445936", "628123456789"} {
		if strings.Contains(reason, secret) {
			t.Fatalf("reason leaked %q: %q", secret, reason)
		}
	}
}

func TestLoginFailureReasonCollectsAllMatchingMessages(t *testing.T) {
	httpErr := &gopay.HTTPError{
		StatusCode: http.StatusForbidden,
		Body: []byte(`{"errors":[` +
			`{"code":"GoPay-112"},` +
			`{"code":"GoPay-112","message_title":"Akun diblokir","message":"Coba lagi nanti"}` +
			`]}`),
	}
	err := fmt.Errorf("%w: %w", gopay.ErrLoginFailed, httpErr)

	reason := loginFailureReason(domain.ActivationStatusSettingPIN, err)
	for _, want := range []string{"错误码：GoPay-112", "Akun diblokir", "Coba lagi nanti"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("reason=%q, want it to contain %q", reason, want)
		}
	}
}

func TestLoginFailureReasonKeepsMatchingErrorItemOnly(t *testing.T) {
	httpErr := &gopay.HTTPError{
		StatusCode: http.StatusTooManyRequests,
		Body:       []byte(`{"errors":[{"code":"unrelated","message":"ignore me"},{"code":"auth:error:ratelimited","message":"Coba lagi nanti"}]}`),
	}
	err := fmt.Errorf("%w: %w", gopay.ErrLoginFailed, httpErr)

	reason := loginFailureReason(domain.ActivationStatusCheckingBalance, err)
	if strings.Contains(reason, "ignore me") {
		t.Fatalf("reason exposed an unrelated upstream item: %q", reason)
	}
	for _, want := range []string{"阶段：余额及 PIN 状态检查", "HTTP 429", "错误码：auth:error:ratelimited", "Coba lagi nanti"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("reason=%q, want it to contain %q", reason, want)
		}
	}
}

func TestLoginFailureReasonExplainsKnownCodeWithoutUpstreamMessage(t *testing.T) {
	httpErr := &gopay.HTTPError{
		StatusCode: http.StatusForbidden,
		Body:       []byte(`{"errors":[{"code":"GoPay-112"}]}`),
	}
	err := fmt.Errorf("%w: %w", gopay.ErrLoginFailed, httpErr)

	reason := loginFailureReason(domain.ActivationStatusAwaitingPINCode, err)
	for _, want := range []string{
		"GoPay 操作失败",
		"阶段：改 PIN 验证",
		"HTTP 403",
		"错误码：GoPay-112",
		"说明：账号或当前操作被 GoPay 暂时限制",
	} {
		if !strings.Contains(reason, want) {
			t.Fatalf("reason=%q, want it to contain %q", reason, want)
		}
	}
}

func TestLoginFailureReasonIsBoundedAndValidUTF8(t *testing.T) {
	httpErr := &gopay.HTTPError{
		StatusCode: http.StatusForbidden,
		Body:       []byte(`{"errors":[{"code":"GoPay-112","message":"` + strings.Repeat("账号暂时受限", 100) + `"}]}`),
	}
	err := fmt.Errorf("%w: %w", gopay.ErrLoginFailed, httpErr)

	reason := loginFailureReason(domain.ActivationStatusAwaitingPINCode, err)
	if len(reason) > maxActivationFailureReasonBytes {
		t.Fatalf("reason length=%d, want <=%d", len(reason), maxActivationFailureReasonBytes)
	}
	if !utf8.ValidString(reason) {
		t.Fatalf("reason is not valid UTF-8: %q", reason)
	}
	if !strings.HasSuffix(reason, "…") {
		t.Fatalf("bounded reason=%q, want ellipsis", reason)
	}
}

func TestLoginFailureReasonWithoutHTTPErrorStillNamesStage(t *testing.T) {
	reason := loginFailureReason(domain.ActivationStatusAwaitingLoginCode, errors.New("wrapped login failure"))
	if reason != "GoPay 登录失败（阶段：登录验证码验证）" {
		t.Fatalf("reason=%q", reason)
	}

	reason = loginFailureReason(domain.ActivationStatusLoggingIn, errors.New("wrapped login failure"))
	if reason != "GoPay 登录失败（阶段：登录验证）" {
		t.Fatalf("reason=%q", reason)
	}
}

func TestLoginFailureReasonHandlesUnstructuredAndInvalidBodies(t *testing.T) {
	for _, body := range [][]byte{
		nil,
		[]byte("not JSON"),
		{0xff, 0xfe},
	} {
		httpErr := &gopay.HTTPError{StatusCode: http.StatusForbidden, Body: body}
		err := fmt.Errorf("%w: %w", gopay.ErrLoginFailed, httpErr)
		reason := loginFailureReason(domain.ActivationStatusCheckingBalance, err)
		if reason != "GoPay 操作失败（阶段：余额及 PIN 状态检查；HTTP 403）" {
			t.Fatalf("body=%q reason=%q", body, reason)
		}
	}
}

func TestBoundActivationFailureReasonRepairsInvalidUTF8(t *testing.T) {
	reason := boundActivationFailureReason(string([]byte{'o', 'k', 0xff}))
	if !utf8.ValidString(reason) {
		t.Fatalf("reason is not valid UTF-8: %q", reason)
	}
}

func TestActivationStepFailureReasonPreservesClassificationsAwaitingProviderFinalization(t *testing.T) {
	for _, test := range []struct {
		status domain.ActivationStatus
		reason string
	}{
		{status: domain.ActivationStatusDuplicate, reason: "重复号码"},
		{status: domain.ActivationStatusPhoneInUse, reason: "该号码当前仍有未结束任务"},
		{status: domain.ActivationStatusPINRequired, reason: "账号需要已有 PIN 登录"},
		{status: domain.ActivationStatusUnregistered, reason: "未注册"},
		{status: domain.ActivationStatusLoginFailed, reason: "GoPay 登录失败（阶段：改 PIN 验证；HTTP 403；错误码：GoPay-112）"},
		{status: domain.ActivationStatusLoginCodeTimeout, reason: "登录验证码重发 3 次后仍未收到"},
		{status: domain.ActivationStatusPINCodeTimeout, reason: "改 PIN 验证码重发 3 次后仍未收到"},
		{status: domain.ActivationStatusZeroBalanceUsed, reason: "0RP已被使用"},
		{status: domain.ActivationStatusPINSubmissionBlocked, reason: "GoPay 操作失败（阶段：提交新 PIN；HTTP 403；错误码：GoPay-112）"},
	} {
		t.Run(string(test.status), func(t *testing.T) {
			activation := domain.Activation{Status: test.status, FailureReason: test.reason}
			providerErr := errors.New("smsbower setStatus: HTTP_409: Conflict")

			if reason := activationStepFailureReason(activation, providerErr); reason != test.reason {
				t.Fatalf("reason=%q, want original classification %q", reason, test.reason)
			}
		})
	}
}

func TestRepairedProviderFinalizationReasonRestoresOnlyFixedLegacyClassifications(t *testing.T) {
	for _, test := range []struct {
		name       string
		activation domain.Activation
		want       string
		wantRepair bool
	}{
		{
			name: "unregistered Hero conflict",
			activation: domain.Activation{
				Status: domain.ActivationStatusUnregistered, FailureReason: "smsbower setStatus: HTTP_409: Conflict",
			},
			want: "未注册", wantRepair: true,
		},
		{
			name: "duplicate provider error",
			activation: domain.Activation{
				Status: domain.ActivationStatusDuplicate, FailureReason: "smsbower setStatus: HTTP_503: Service Unavailable",
			},
			want: "historical phone number", wantRepair: true,
		},
		{
			name: "PIN required empty response",
			activation: domain.Activation{
				Status: domain.ActivationStatusPINRequired, FailureReason: "smsbower setStatus: EMPTY_RESPONSE",
			},
			want: "账号需要已有 PIN 登录", wantRepair: true,
		},
		{
			name: "zero balance provider error",
			activation: domain.Activation{
				Status: domain.ActivationStatusZeroBalanceUsed, FailureReason: "smsbower setStatus request: temporary transport error",
			},
			want: "0RP已被使用", wantRepair: true,
		},
		{
			name: "valid unregistered detail",
			activation: domain.Activation{
				Status: domain.ActivationStatusUnregistered, FailureReason: "未注册（人工复核）",
			},
		},
		{
			name: "variable login failure detail",
			activation: domain.Activation{
				Status: domain.ActivationStatusLoginFailed, FailureReason: "smsbower setStatus: HTTP_409: Conflict",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, repair := repairedProviderFinalizationReason(test.activation)
			if got != test.want || repair != test.wantRepair {
				t.Fatalf("repairedProviderFinalizationReason()=(%q, %v), want (%q, %v)", got, repair, test.want, test.wantRepair)
			}
		})
	}
}

func TestPINSubmissionBlockedFailureRequiresExactStructuredMatch(t *testing.T) {
	matchingHTTPError := &gopay.HTTPError{
		StatusCode: http.StatusForbidden,
		Body:       []byte(`{"errors":[{"code":"GoPay-112"}]}`),
	}
	matching := fmt.Errorf("complete PIN: %w: %w", gopay.ErrLoginFailed, matchingHTTPError)

	for _, test := range []struct {
		name   string
		status domain.ActivationStatus
		err    error
		want   bool
	}{
		{name: "exact setting PIN failure", status: domain.ActivationStatusSettingPIN, err: matching, want: true},
		{name: "different activation stage", status: domain.ActivationStatusAwaitingPINCode, err: matching},
		{name: "HTTP error not retained", status: domain.ActivationStatusSettingPIN, err: gopay.ErrLoginFailed},
		{name: "wrong HTTP status", status: domain.ActivationStatusSettingPIN, err: fmt.Errorf("%w: %w", gopay.ErrLoginFailed, &gopay.HTTPError{StatusCode: http.StatusTooManyRequests, Body: matchingHTTPError.Body})},
		{name: "case variant code", status: domain.ActivationStatusSettingPIN, err: fmt.Errorf("%w: %w", gopay.ErrLoginFailed, &gopay.HTTPError{StatusCode: http.StatusForbidden, Body: []byte(`{"errors":[{"code":"gopay-112"}]}`)})},
		{name: "space padded code", status: domain.ActivationStatusSettingPIN, err: fmt.Errorf("%w: %w", gopay.ErrLoginFailed, &gopay.HTTPError{StatusCode: http.StatusForbidden, Body: []byte(`{"errors":[{"code":" GoPay-112 "}]}`)})},
		{name: "code outside errors array", status: domain.ActivationStatusSettingPIN, err: fmt.Errorf("%w: %w", gopay.ErrLoginFailed, &gopay.HTTPError{StatusCode: http.StatusForbidden, Body: []byte(`{"code":"GoPay-112"}`)})},
		{name: "code only in message", status: domain.ActivationStatusSettingPIN, err: fmt.Errorf("%w: %w", gopay.ErrLoginFailed, &gopay.HTTPError{StatusCode: http.StatusForbidden, Body: []byte(`{"errors":[{"code":"GoPay-113","message":"GoPay-112"}]}`)})},
		{name: "invalid JSON", status: domain.ActivationStatusSettingPIN, err: fmt.Errorf("%w: %w", gopay.ErrLoginFailed, &gopay.HTTPError{StatusCode: http.StatusForbidden, Body: []byte(`not JSON`)})},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := pinSubmissionBlockedFailure(test.status, test.err); got != test.want {
				t.Fatalf("pinSubmissionBlockedFailure()=%v, want %v", got, test.want)
			}
		})
	}
}

func TestActivationStepFailureReasonUsesCurrentErrorOutsideLoginFailureFinalization(t *testing.T) {
	activation := domain.Activation{
		Status:        domain.ActivationStatusAwaitingPINCode,
		FailureReason: "stale error",
	}

	if reason := activationStepFailureReason(activation, errors.New("current error")); reason != "current error" {
		t.Fatalf("reason=%q, want current error", reason)
	}
}
