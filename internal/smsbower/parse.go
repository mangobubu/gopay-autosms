package smsbower

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

func decodeJSON(body []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return nil, fmt.Errorf("unexpected trailing JSON value")
	} else if err != io.EOF {
		return nil, fmt.Errorf("unexpected trailing JSON: %w", err)
	}
	return value, nil
}

func parseServices(action string, body []byte) ([]Service, error) {
	if err := providerError(action, body); err != nil {
		return nil, err
	}
	value, err := decodeJSON(body)
	if err != nil {
		return nil, fmt.Errorf("smsbower %s: decode services: %w", action, err)
	}
	value = unwrap(value, "services", "data", "result")
	services := make(map[string]Service)
	var walk func(any, string)
	walk = func(node any, keyHint string) {
		switch node := node.(type) {
		case []any:
			for _, item := range node {
				walk(item, "")
			}
		case map[string]any:
			code, _ := lookupString(node, "code", "serviceCode", "service")
			name, _ := lookupString(node, "name", "title", "eng", "rus")
			if code == "" && name != "" && !isMetadataKey(keyHint) {
				code = keyHint
			}
			if code != "" && name != "" {
				code = strings.TrimSpace(code)
				services[code] = Service{Code: code, Name: strings.TrimSpace(name), Raw: marshalRaw(node)}
				return
			}
			for key, item := range node {
				if isMetadataKey(key) {
					continue
				}
				if scalar, ok := scalarString(item); ok {
					if scalar != "" {
						services[key] = Service{Code: key, Name: scalar, Raw: marshalRaw(item)}
					}
					continue
				}
				walk(item, key)
			}
		}
	}
	walk(value, "")
	result := make([]Service, 0, len(services))
	for _, service := range services {
		result = append(result, service)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Code < result[j].Code })
	return result, nil
}

func parseCountries(action string, body []byte) ([]Country, error) {
	if err := providerError(action, body); err != nil {
		return nil, err
	}
	value, err := decodeJSON(body)
	if err != nil {
		return nil, fmt.Errorf("smsbower %s: decode countries: %w", action, err)
	}
	value = unwrap(value, "countries", "data", "result")
	countries := make(map[int]Country)
	var walk func(any, string)
	walk = func(node any, keyHint string) {
		switch node := node.(type) {
		case []any:
			for _, item := range node {
				walk(item, "")
			}
		case map[string]any:
			id, hasID := lookupInt(node, "id", "countryId", "country")
			if !hasID && keyHint != "" {
				id, hasID = parseInt(keyHint)
			}
			eng, _ := lookupString(node, "eng", "english", "nameEn")
			rus, _ := lookupString(node, "rus", "russian", "nameRu")
			chn, _ := lookupString(node, "chn", "chinese", "nameZh", "zh")
			name, _ := lookupString(node, "name", "title")
			if name == "" {
				name = firstNonEmpty(eng, rus, chn)
			}
			if hasID && name != "" {
				visible, _ := lookupBool(node, "visible")
				retry, _ := lookupBool(node, "retry")
				rent, _ := lookupBool(node, "rent")
				multi, _ := lookupBool(node, "multiService")
				countries[id] = Country{
					ID: id, Name: name, EnglishName: eng, RussianName: rus,
					ChineseName: chn, Visible: visible, Retry: retry, Rent: rent,
					MultiService: multi, Raw: marshalRaw(node),
				}
				return
			}
			for key, item := range node {
				if !isMetadataKey(key) {
					if id, validID := parseInt(key); validID {
						if scalar, scalarOK := scalarString(item); scalarOK && scalar != "" {
							countries[id] = Country{ID: id, Name: scalar, Raw: marshalRaw(item)}
							continue
						}
					}
					walk(item, key)
				}
			}
		}
	}
	walk(value, "")
	result := make([]Country, 0, len(countries))
	for _, country := range countries {
		result = append(result, country)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func parsePrices(action string, body []byte) ([]Price, error) {
	if err := providerError(action, body); err != nil {
		return nil, err
	}
	value, err := decodeJSON(body)
	if err != nil {
		return nil, fmt.Errorf("smsbower %s: decode prices: %w", action, err)
	}
	value = unwrap(value, "prices", "data", "result")
	result := make([]Price, 0)
	var walk func(any, []string)
	walk = func(node any, path []string) {
		switch node := node.(type) {
		case []any:
			for _, item := range node {
				walk(item, path)
			}
		case map[string]any:
			price, hasPrice := lookupString(node, "price", "cost", "activationCost", "retailPrice")
			if hasPrice {
				country, service, providerID := pricePath(path)
				if value, ok := lookupInt(node, "country", "countryId"); ok {
					country = value
				}
				if value, ok := lookupString(node, "service", "serviceCode"); ok {
					service = value
				}
				if value, ok := lookupInt64(node, "providerId", "provider"); ok {
					providerID = value
				}
				count, _ := lookupInt(node, "count", "quantity", "available")
				result = append(result, Price{
					Country: country, Service: service, ProviderID: providerID,
					Price: price, Count: count, Raw: marshalRaw(node),
				})
				return
			}
			for key, item := range node {
				if isMetadataKey(key) {
					continue
				}
				// A few compatible gateways use price-as-key/count-as-value.
				if countText, ok := scalarString(item); ok && isPriceKey(key) {
					country, service, providerID := pricePath(path)
					if service == "" {
						continue
					}
					count, _ := strconv.Atoi(countText)
					result = append(result, Price{
						Country: country, Service: service, ProviderID: providerID,
						Price: key, Count: count, Raw: marshalRaw(item),
					})
					continue
				}
				walk(item, append(path, key))
			}
		}
	}
	walk(value, nil)
	if len(result) == 0 && !isExplicitlyEmpty(value) {
		return nil, fmt.Errorf("smsbower %s: unrecognized price catalogue structure", action)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Country != result[j].Country {
			return result[i].Country < result[j].Country
		}
		if result[i].Service != result[j].Service {
			return result[i].Service < result[j].Service
		}
		if result[i].ProviderID != result[j].ProviderID {
			return result[i].ProviderID < result[j].ProviderID
		}
		left, leftErr := strconv.ParseFloat(result[i].Price, 64)
		right, rightErr := strconv.ParseFloat(result[j].Price, 64)
		if leftErr == nil && rightErr == nil && left != right {
			return left < right
		}
		return result[i].Price < result[j].Price
	})
	return result, nil
}

func parseActivation(action string, body []byte) (Activation, error) {
	if err := providerError(action, body); err != nil {
		return Activation{}, err
	}
	text := string(body)
	if strings.HasPrefix(strings.ToUpper(text), "ACCESS_NUMBER:") {
		parts := strings.SplitN(text, ":", 3)
		if len(parts) != 3 || strings.TrimSpace(parts[1]) == "" || strings.TrimSpace(parts[2]) == "" {
			return Activation{}, fmt.Errorf("smsbower %s: malformed ACCESS_NUMBER response", action)
		}
		return Activation{
			ActivationID: strings.TrimSpace(parts[1]),
			PhoneNumber:  normalizePhone(parts[2]),
			Raw:          cloneRaw(body),
		}, nil
	}
	value, err := decodeJSON(body)
	if err != nil {
		return Activation{}, fmt.Errorf("smsbower %s: decode number response: %w", action, err)
	}
	root := unwrap(value, "data", "result", "activation")
	id, _ := recursiveString(root, "activationId", "activation_id", "id")
	phone, _ := recursiveString(root, "phoneNumber", "phone_number", "number", "phone")
	if strings.TrimSpace(id) == "" || strings.TrimSpace(phone) == "" {
		return Activation{}, fmt.Errorf("smsbower %s: number response lacks activationId or phoneNumber", action)
	}
	cost, _ := recursiveString(root, "activationCost", "activation_cost", "cost", "price")
	currency, _ := recursiveString(root, "currency", "currencyCode")
	countryCode, _ := recursiveString(root, "countryCode", "country_code")
	operator, _ := recursiveString(root, "activationOperator", "activation_operator", "operator")
	canAnother, _ := recursiveBool(root, "canGetAnotherSms", "can_get_another_sms")
	timeText, _ := recursiveString(root, "activationTime", "activation_time", "createdAt")
	return Activation{
		ActivationID: id, PhoneNumber: normalizePhone(phone), Cost: cost,
		Currency: currency, CountryCode: countryCode, CanGetAnotherSMS: canAnother,
		ActivatedAt: parseTime(timeText), Operator: operator, Raw: cloneRaw(body),
	}, nil
}

func parseActivationStatus(action string, body []byte) (ActivationStatus, error) {
	if status, ok := statusFromText(string(body)); ok {
		status.Raw = cloneRaw(body)
		return status, nil
	}
	if err := providerError(action, body); err != nil {
		return ActivationStatus{}, err
	}
	value, err := decodeJSON(body)
	if err != nil {
		return ActivationStatus{}, fmt.Errorf("smsbower %s: decode status: %w", action, err)
	}
	var statusText string
	findScalar(value, func(_ string, scalar string) bool {
		upper := strings.ToUpper(strings.TrimSpace(scalar))
		if strings.HasPrefix(upper, "STATUS_") {
			statusText = scalar
			return true
		}
		return false
	})
	if statusText != "" {
		if status, ok := statusFromText(statusText); ok {
			if status.Code == "" {
				status.Code, _ = recursiveString(value, "code", "smsCode", "sms_code")
			}
			status.Raw = cloneRaw(body)
			return status, nil
		}
	}
	state, _ := recursiveString(value, "status", "state")
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "cancel", "cancelled", "canceled":
		return ActivationStatus{Kind: StatusCancel, Raw: cloneRaw(body)}, nil
	case "success", "ok", "received":
		code, _ := recursiveString(value, "code", "smsCode", "sms_code")
		return ActivationStatus{Kind: StatusOK, Code: code, Raw: cloneRaw(body)}, nil
	case "waiting", "wait", "pending":
		return ActivationStatus{Kind: StatusWaitCode, Raw: cloneRaw(body)}, nil
	}
	return ActivationStatus{Kind: StatusUnknown, Raw: cloneRaw(body)}, nil
}

func statusFromText(text string) (ActivationStatus, bool) {
	text = strings.TrimSpace(text)
	upper := strings.ToUpper(text)
	code := ""
	if index := strings.IndexByte(text, ':'); index >= 0 {
		code = strings.TrimSpace(text[index+1:])
		upper = strings.ToUpper(strings.TrimSpace(text[:index]))
	}
	switch upper {
	case "STATUS_WAIT_CODE":
		return ActivationStatus{Kind: StatusWaitCode}, true
	case "STATUS_WAIT_RETRY":
		return ActivationStatus{Kind: StatusWaitRetry, Code: code}, true
	case "STATUS_WAIT_RESEND":
		return ActivationStatus{Kind: StatusWaitResend, Code: code}, true
	case "STATUS_OK":
		return ActivationStatus{Kind: StatusOK, Code: code}, true
	case "STATUS_CANCEL":
		return ActivationStatus{Kind: StatusCancel}, true
	default:
		return ActivationStatus{}, false
	}
}

func parseSetStatus(action string, body []byte) (SetStatusResult, error) {
	if err := providerError(action, body); err != nil {
		return SetStatusResult{}, err
	}
	text := strings.TrimSpace(string(body))
	if strings.HasPrefix(strings.ToUpper(text), "ACCESS_") {
		return SetStatusResult{Code: text, Raw: cloneRaw(body)}, nil
	}
	value, err := decodeJSON(body)
	if err != nil {
		return SetStatusResult{}, fmt.Errorf("smsbower %s: decode setStatus: %w", action, err)
	}
	var code string
	findScalar(value, func(_ string, scalar string) bool {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(scalar)), "ACCESS_") {
			code = scalar
			return true
		}
		return false
	})
	if code == "" {
		code, _ = recursiveString(unwrap(value, "data", "result"), "code", "status", "result")
	}
	if code == "" {
		return SetStatusResult{}, fmt.Errorf("smsbower %s: response lacks acknowledgement", action)
	}
	return SetStatusResult{Code: code, Raw: cloneRaw(body)}, nil
}

func providerError(action string, body []byte) error {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return &APIError{Action: action, Code: "EMPTY_RESPONSE"}
	}
	if text[0] != '{' && text[0] != '[' {
		code, message := splitProviderMessage(text)
		if isProviderErrorCode(code) {
			return &APIError{Action: action, Code: code, Message: message, Raw: text}
		}
		return nil
	}
	value, err := decodeJSON(body)
	if err != nil {
		return nil // The action-specific decoder will return the useful JSON error.
	}
	var foundCode, foundMessage string
	// Prefer the specific error fields over a generic {"status":"error"}.
	for _, key := range []string{"errorCode", "error", "code", "status"} {
		scalar, ok := recursiveString(value, key)
		if !ok {
			continue
		}
		code, message := splitProviderMessage(scalar)
		if isProviderErrorCode(code) {
			foundCode, foundMessage = code, message
			break
		}
	}
	if foundCode == "" {
		for _, key := range []string{"message", "description", "errorMessage"} {
			message, ok := recursiveString(value, key)
			if !ok {
				continue
			}
			code, detail := splitProviderMessage(message)
			if isProviderErrorCode(code) {
				foundCode, foundMessage = code, detail
				break
			}
		}
	}
	if foundCode == "ERROR" {
		if message, ok := recursiveString(value, "message", "description", "errorMessage"); ok {
			code, detail := splitProviderMessage(message)
			if isProviderErrorCode(code) {
				foundCode, foundMessage = code, detail
			}
		}
	}
	if foundCode == "" {
		return nil
	}
	if message, ok := recursiveString(value, "message", "description", "errorMessage"); ok && message != "" {
		foundMessage = message
	}
	return &APIError{Action: action, Code: foundCode, Message: foundMessage, Raw: text}
}

func splitProviderMessage(text string) (string, string) {
	text = strings.TrimSpace(text)
	for _, separator := range []string{":", " ", "\t", "\n"} {
		if index := strings.Index(text, separator); index > 0 {
			return strings.ToUpper(strings.TrimSpace(text[:index])), strings.TrimSpace(text[index+len(separator):])
		}
	}
	return strings.ToUpper(text), ""
}

func isProviderErrorCode(code string) bool {
	code = strings.ToUpper(strings.TrimSpace(code))
	prefixes := [...]string{
		"BAD_", "NO_", "NOT_", "ERROR", "WRONG_", "INVALID_", "EARLY_",
		"ACCOUNT_", "BANNED", "MAX_", "CANNOT_", "EXCEPTION_", "HTTP_",
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(code, prefix) {
			return true
		}
	}
	return false
}

func unwrap(value any, keys ...string) any {
	for {
		object, ok := value.(map[string]any)
		if !ok {
			return value
		}
		changed := false
		for _, key := range keys {
			if child, exists := lookup(object, key); exists {
				switch child.(type) {
				case map[string]any, []any:
					value, changed = child, true
				}
				if changed {
					break
				}
			}
		}
		if !changed {
			return value
		}
	}
}

func lookup(object map[string]any, names ...string) (any, bool) {
	for _, name := range names {
		wanted := normalizeKey(name)
		for key, value := range object {
			if normalizeKey(key) == wanted {
				return value, true
			}
		}
	}
	return nil, false
}

func lookupString(object map[string]any, names ...string) (string, bool) {
	value, ok := lookup(object, names...)
	if !ok {
		return "", false
	}
	return scalarString(value)
}

func lookupInt(object map[string]any, names ...string) (int, bool) {
	value, ok := lookupString(object, names...)
	if !ok {
		return 0, false
	}
	return parseInt(value)
}

func lookupInt64(object map[string]any, names ...string) (int64, bool) {
	value, ok := lookupString(object, names...)
	if !ok {
		return 0, false
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	return parsed, err == nil
}

func lookupBool(object map[string]any, names ...string) (bool, bool) {
	value, ok := lookup(object, names...)
	if !ok {
		return false, false
	}
	return boolValue(value)
}

func recursiveString(value any, names ...string) (string, bool) {
	wanted := make(map[string]struct{}, len(names))
	for _, name := range names {
		wanted[normalizeKey(name)] = struct{}{}
	}
	var result string
	found := findScalar(value, func(key, scalar string) bool {
		if _, ok := wanted[normalizeKey(key)]; ok {
			result = scalar
			return true
		}
		return false
	})
	return result, found
}

func recursiveBool(value any, names ...string) (bool, bool) {
	wanted := make(map[string]struct{}, len(names))
	for _, name := range names {
		wanted[normalizeKey(name)] = struct{}{}
	}
	var result bool
	var found bool
	var walk func(any) bool
	walk = func(node any) bool {
		switch node := node.(type) {
		case map[string]any:
			for key, item := range node {
				if _, ok := wanted[normalizeKey(key)]; ok {
					if result, found = boolValue(item); found {
						return true
					}
				}
			}
			for _, item := range node {
				if walk(item) {
					return true
				}
			}
		case []any:
			for _, item := range node {
				if walk(item) {
					return true
				}
			}
		}
		return false
	}
	walk(value)
	return result, found
}

func findScalar(value any, visit func(key, scalar string) bool) bool {
	switch value := value.(type) {
	case map[string]any:
		for key, item := range value {
			if scalar, ok := scalarString(item); ok && visit(key, scalar) {
				return true
			}
		}
		for _, item := range value {
			if findScalar(item, visit) {
				return true
			}
		}
	case []any:
		for _, item := range value {
			if findScalar(item, visit) {
				return true
			}
		}
	}
	return false
}

func scalarString(value any) (string, bool) {
	switch value := value.(type) {
	case string:
		return strings.TrimSpace(value), true
	case json.Number:
		return value.String(), true
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64), true
	case float32:
		return strconv.FormatFloat(float64(value), 'f', -1, 32), true
	case int:
		return strconv.Itoa(value), true
	case int64:
		return strconv.FormatInt(value, 10), true
	case bool:
		return strconv.FormatBool(value), true
	default:
		return "", false
	}
}

func boolValue(value any) (bool, bool) {
	switch value := value.(type) {
	case bool:
		return value, true
	case json.Number:
		parsed, err := strconv.ParseInt(value.String(), 10, 64)
		return parsed != 0, err == nil
	case string:
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "1", "true", "yes", "on":
			return true, true
		case "0", "false", "no", "off", "":
			return false, true
		}
	}
	return false, false
}

func pricePath(path []string) (country int, service string, providerID int64) {
	countryIndex := -1
	for index, segment := range path {
		if value, ok := parseInt(segment); ok {
			country, countryIndex = value, index
			break
		}
	}
	for index := countryIndex + 1; index < len(path); index++ {
		segment := path[index]
		if isMetadataKey(segment) {
			continue
		}
		if value, err := strconv.ParseInt(segment, 10, 64); err == nil {
			if service != "" {
				providerID = value
			}
			continue
		}
		if service == "" {
			service = segment
		}
	}
	return country, service, providerID
}

func normalizeKey(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, value)
}

func isMetadataKey(key string) bool {
	switch normalizeKey(key) {
	case "status", "success", "message", "currency", "version", "timestamp", "meta":
		return true
	default:
		return false
	}
}

func parseInt(value string) (int, bool) {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	return parsed, err == nil
}

func isPriceKey(value string) bool {
	if strings.TrimSpace(value) == "" {
		return false
	}
	_, err := strconv.ParseFloat(strings.ReplaceAll(value, ",", "."), 64)
	return err == nil
}

func isExplicitlyEmpty(value any) bool {
	switch value := value.(type) {
	case map[string]any:
		return len(value) == 0
	case []any:
		return len(value) == 0
	default:
		return false
	}
}

func normalizePhone(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "+") {
		return value
	}
	return "+" + value
}

func parseTime(value string) time.Time {
	value = strings.TrimSpace(value)
	if unixSeconds, err := strconv.ParseInt(value, 10, 64); err == nil && unixSeconds > 0 {
		return time.Unix(unixSeconds, 0).UTC()
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
	} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func marshalRaw(value any) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}

func cloneRaw(value []byte) json.RawMessage {
	return append(json.RawMessage(nil), value...)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
