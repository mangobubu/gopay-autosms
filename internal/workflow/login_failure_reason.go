package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	// Classified provider actions are finalized on a later worker pass. A
	// transient cancellation or completion error must not replace the durable
	// classification already shown on the activation.
	if repairedReason, repair := repairedProviderFinalizationReason(activation); repair {
		return boundActivationFailureReason(repairedReason)
	}
	if awaitingProviderFinalization(activation.Status) &&
		strings.TrimSpace(activation.FailureReason) != "" {
		return boundActivationFailureReason(activation.FailureReason)
	}
	return boundActivationFailureReason(err.Error())
}

func awaitingProviderFinalization(status domain.ActivationStatus) bool {
	switch status {
	case domain.ActivationStatusDuplicate,
		domain.ActivationStatusPhoneInUse,
		domain.ActivationStatusPINRequired,
		domain.ActivationStatusUnregistered,
		domain.ActivationStatusLoginFailed,
		domain.ActivationStatusLoginCodeTimeout,
		domain.ActivationStatusPINCodeTimeout,
		domain.ActivationStatusZeroBalanceUsed,
		domain.ActivationStatusPINSubmissionBlocked:
		return true
	default:
		return false
	}
}

func repairedProviderFinalizationReason(activation domain.Activation) (string, bool) {
	// Older workers persisted the provider retry error over fixed business
	// classifications which were still waiting for setStatus to succeed. Repair
	// only that recognizable legacy shape; preserve every other stored detail.
	legacyReason := strings.ToLower(strings.TrimSpace(activation.FailureReason))
	if !strings.HasPrefix(legacyReason, "smsbower setstatus") {
		return "", false
	}
	switch activation.Status {
	case domain.ActivationStatusDuplicate:
		return "historical phone number", true
	case domain.ActivationStatusPINRequired:
		return "账号需要已有 PIN 登录", true
	case domain.ActivationStatusUnregistered:
		return "未注册", true
	case domain.ActivationStatusZeroBalanceUsed:
		return "0RP已被使用", true
	default:
		return "", false
	}
}

func pinSubmissionBlockedFailure(status domain.ActivationStatus, err error) bool {
	if status != domain.ActivationStatusSettingPIN {
		return false
	}
	var httpErr *gopay.HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusForbidden {
		return false
	}
	var payload struct {
		Errors []struct {
			Code string `json:"code"`
		} `json:"errors"`
	}
	if json.Unmarshal(httpErr.Body, &payload) != nil {
		return false
	}
	for _, item := range payload.Errors {
		if item.Code == "GoPay-112" {
			return true
		}
	}
	return false
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
