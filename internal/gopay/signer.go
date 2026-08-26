package gopay

import (
	"crypto/hmac"
	"crypto/md5" // #nosec G501 -- the upstream protocol requires MD5 in its canonical body digest.
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	SignatureV2ID  = "ED9A2B38749FBDE9ACA61D6A685B7"
	defaultHMACKey = "4&G6DbV&j8QZs~{)(Ila_w_|v@aqJq]E-;*(J9PanZ8sm01kTi{X<iG``]d7P&L"
)

// SignInput is the complete canonical input for GoPay's X-E1/X-E2 signature.
// URL is host+path, without scheme (for example
// "accounts.goto-products.com/goto-auth/login/methods").
type SignInput struct {
	Token       string
	TimestampMS string
	URL         string
	Method      string
	Body        string
	Model       string
	XM1         string
	OSInfo      string
	AppID       string
	Version     string
	UniqueID    string
	NonceHex    string
	PhoneMake   string
	OSName      string
	Adjustment  string
	HMACKey     []byte
}

type Signature struct {
	XE1      string
	XE2      string
	HMAC     string
	Message  string
	BodyHash string
}

// SignV2 implements the capture-verified GoPay 2.8.0 canonical signature.
func SignV2(in SignInput) (Signature, error) {
	if in.TimestampMS == "" {
		return Signature{}, fmt.Errorf("gopay: signing timestamp is required")
	}
	if in.NonceHex == "" {
		return Signature{}, fmt.Errorf("gopay: signing nonce is required")
	}
	if _, err := hex.DecodeString(in.NonceHex); err != nil {
		return Signature{}, fmt.Errorf("gopay: signing nonce must be hexadecimal: %w", err)
	}
	if strings.HasPrefix(in.Token, "Bearer ") {
		in.Token = strings.TrimPrefix(in.Token, "Bearer ")
	}
	if in.Method == "" {
		in.Method = "GET"
	}
	if in.Model == "" {
		in.Model = "Xiaomi, MI 9"
	}
	if in.OSInfo == "" {
		in.OSInfo = "Android,13"
	}
	if in.AppID == "" {
		in.AppID = DefaultAppID
	}
	if in.Version == "" {
		in.Version = DefaultAppVersion
	}
	if in.PhoneMake == "" {
		in.PhoneMake = "Google"
	}
	if in.OSName == "" {
		in.OSName = "Android"
	}
	if in.Adjustment == "" {
		in.Adjustment = "D"
	}

	bodySum := md5.Sum([]byte(in.Body)) // #nosec G401 -- protocol compatibility, not password hashing.
	bodyHash := hex.EncodeToString(bodySum[:])
	message := "GOPAY;" +
		in.Model + ":" + in.Token + ";" +
		in.UniqueID + ":;" +
		bodyHash + ":" + in.URL + ";" +
		strings.ToUpper(in.Method) + ":" + in.TimestampMS + ";" +
		in.OSInfo + ":" + in.Version + ";" +
		in.XM1 + ":" + in.AppID + ";" +
		in.NonceHex + ":" + in.PhoneMake + ";" +
		in.OSName

	key := in.HMACKey
	if len(key) == 0 {
		key = []byte(defaultHMACKey)
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(message))
	macHex := hex.EncodeToString(mac.Sum(nil))

	return Signature{
		XE1:      macHex + ":" + in.NonceHex + ":" + in.Adjustment + ":" + in.TimestampMS,
		XE2:      SignatureV2ID,
		HMAC:     macHex,
		Message:  message,
		BodyHash: bodyHash,
	}, nil
}
