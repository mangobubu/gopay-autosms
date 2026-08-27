package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/mangobubu/gopay-autosms/internal/domain"
	"github.com/mangobubu/gopay-autosms/internal/gopay"
)

const maxActivationFailureReasonBytes = 500

type upstreamLoginFailure struct {
	Code         string `json:"code"`
	Message      string `json:"message"`
	MessageTitle string `json:"message_title"`
}

func activationStepFailureReason(activation domain.Activation, err error) string {
	// Classified cancellations are finalized with the SMS provider on a later
	// worker pass. A transient cancellation error must not replace the durable
	// classification already shown on the activation.
	if (activation.Status == domain.ActivationStatusLoginFailed ||
		activation.Status == domain.ActivationStatusLoginCodeTimeout) &&
		strings.TrimSpace(activation.FailureReason) != "" {
		return boundActivationFailureReason(activation.FailureReason)
	}
	return boundActivationFailureReason(err.Error())
}

func loginFailureReason(status domain.ActivationStatus, err error) string {
	parts := []string{
		loginFailureSummary(status),
		"阶段：" + loginFailureStage(status),
	}

	var httpErr *gopay.HTTPError
	if errors.As(err, &httpErr) {
		parts = append(parts, fmt.Sprintf("HTTP %d", httpErr.StatusCode))
		if code, message := loginFailureDetail(httpErr); code != "" || message != "" {
			if code != "" {
				parts = append(parts, "错误码："+code)
			}
			if message != "" {
				parts = append(parts, "GoPay 信息："+message)
			} else if explanation := loginFailureExplanation(code); explanation != "" {
				parts = append(parts, "说明："+explanation)
			}
		}
	}

	return boundActivationFailureReason(parts[0] + "（" + strings.Join(parts[1:], "；") + "）")
}

func loginFailureExplanation(code string) string {
	switch {
	case strings.EqualFold(code, "GoPay-112"):
		return "账号或当前操作被 GoPay 暂时限制"
	case strings.EqualFold(code, "auth:error:ratelimited"):
		return "GoPay 登录请求频率受限"
	default:
		return ""
	}
}

func loginFailureSummary(status domain.ActivationStatus) string {
	switch status {
	case domain.ActivationStatusPurchased,
		domain.ActivationStatusAwaitingLoginCode,
		domain.ActivationStatusLoggingIn:
		return "GoPay 登录失败"
	case domain.ActivationStatusCheckingBalance,
		domain.ActivationStatusAwaitingPINCode,
		domain.ActivationStatusSettingPIN:
		return "GoPay 操作失败"
	default:
		return "GoPay 请求失败"
	}
}

func loginFailureStage(status domain.ActivationStatus) string {
	switch status {
	case domain.ActivationStatusPurchased:
		return "登录初始化"
	case domain.ActivationStatusAwaitingLoginCode:
		return "登录验证码验证"
	case domain.ActivationStatusLoggingIn:
		return "登录验证"
	case domain.ActivationStatusCheckingBalance:
		return "余额及 PIN 状态检查"
	case domain.ActivationStatusAwaitingPINCode:
		return "改 PIN 验证"
	case domain.ActivationStatusSettingPIN:
		return "提交新 PIN"
	default:
		return string(status)
	}
}

func loginFailureDetail(httpErr *gopay.HTTPError) (string, string) {
	var payload struct {
		Errors []upstreamLoginFailure `json:"errors"`
	}
	if err := json.Unmarshal(httpErr.Body, &payload); err != nil {
		return "", ""
	}

	expectedCode := ""
	switch httpErr.StatusCode {
	case 403:
		expectedCode = "GoPay-112"
	case 429:
		expectedCode = "auth:error:ratelimited"
	}
	codes := make([]string, 0, len(payload.Errors))
	messages := make([]string, 0, len(payload.Errors)*2)
	for _, item := range payload.Errors {
		code := oneLine(item.Code)
		if expectedCode == "" || !strings.EqualFold(code, expectedCode) {
			continue
		}
		codes = append(codes, code)
		messages = append(messages,
			gopay.RedactErrorDetail(oneLine(item.MessageTitle)),
			gopay.RedactErrorDetail(oneLine(item.Message)),
		)
	}
	return strings.Join(uniqueNonEmpty(codes...), " / "), strings.Join(uniqueNonEmpty(messages...), " / ")
}

func oneLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func uniqueNonEmpty(values ...string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func boundActivationFailureReason(value string) string {
	value = strings.ToValidUTF8(value, "�")
	if len(value) <= maxActivationFailureReasonBytes {
		return value
	}
	const suffix = "…"
	value = value[:maxActivationFailureReasonBytes-len(suffix)]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value + suffix
}
