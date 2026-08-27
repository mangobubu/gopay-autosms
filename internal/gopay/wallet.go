package gopay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// GetBalance reads the GoPay wallet balance. A successful-but-unrecognised
// payload returns ErrBalanceUnknown and Known=false; it never silently becomes
// a zero balance.
func (c *Client) GetBalance(ctx context.Context) (BalanceResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session.AccessToken == "" {
		return BalanceResult{}, fmt.Errorf("%w: access token is missing", ErrInvalidState)
	}
	response, err := c.gopayRequest(ctx, "GET", "/v1/payment-options/balances", nil, nil)
	if err != nil {
		result := BalanceResult{Raw: append([]byte(nil), response.body...)}
		if loginFailureError(err) {
			return result, fmt.Errorf("%w: %w", ErrLoginFailed, err)
		}
		if response.status >= 200 && response.status < 300 {
			return result, fmt.Errorf("%w: %v", ErrBalanceUnknown, err)
		}
		return result, err
	}
	result := BalanceResult{Raw: append([]byte(nil), response.body...)}
	data, ok := response.json["data"].([]any)
	if !ok || len(data) == 0 {
		return result, fmt.Errorf("%w: data is missing or empty", ErrBalanceUnknown)
	}
	type candidate struct {
		amount   int64
		currency string
		score    int
	}
	var best *candidate
	for _, entryValue := range data {
		entry, ok := entryValue.(map[string]any)
		if !ok {
			continue
		}
		balance, ok := entry["balance"].(map[string]any)
		if !ok {
			continue
		}
		amount, err := parseAmount(balance["value"])
		if err != nil {
			return result, fmt.Errorf("%w: %v", ErrBalanceUnknown, err)
		}
		currency := firstString(balance, "currency", "currency_code", "unit")
		if currency == "" {
			currency = firstString(entry, "currency", "currency_code", "currencyCode")
		}
		text := strings.ToLower(fmt.Sprint(entry["type"], " ", entry["name"], " ", entry["payment_method"], " ", entry["paymentMethod"]))
		score := 0
		if strings.Contains(text, "gopay") || strings.Contains(text, "wallet") {
			score += 3
		}
		if strings.Contains(strings.ToLower(currency), "idr") || strings.Contains(strings.ToLower(currency), "rp") {
			score += 2
		}
		if best == nil || score > best.score {
			best = &candidate{amount: amount, currency: currency, score: score}
		}
	}
	if best != nil {
		// A lone unlabelled entry is accepted for compatibility with the
		// attachment response; multiple equally unlabelled payment entries are
		// ambiguous and must not be mistaken for 0 RP.
		if len(data) > 1 && best.score == 0 {
			return result, fmt.Errorf("%w: wallet entry is ambiguous", ErrBalanceUnknown)
		}
		result.Amount, result.Currency, result.Known = best.amount, best.currency, true
		return result, nil
	}
	return result, fmt.Errorf("%w: balance.value is missing", ErrBalanceUnknown)
}

func parseAmount(value any) (int64, error) {
	switch v := value.(type) {
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n, nil
		}
		f, err := v.Float64()
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) || f > math.MaxInt64 || f < math.MinInt64 {
			return 0, fmt.Errorf("invalid numeric balance %q", v)
		}
		return int64(f), nil
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || v != math.Trunc(v) || v > math.MaxInt64 || v < math.MinInt64 {
			return 0, fmt.Errorf("invalid numeric balance %v", v)
		}
		return int64(v), nil
	case string:
		text := strings.TrimSpace(v)
		if text == "" {
			return 0, errors.New("empty balance")
		}
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid balance %q", text)
		}
		return n, nil
	default:
		return 0, fmt.Errorf("unsupported balance type %T", value)
	}
}

func firstString(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if text, ok := value[key].(string); ok && text != "" {
			return text
		}
	}
	return ""
}
