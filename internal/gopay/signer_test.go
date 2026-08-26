package gopay

import (
	"strings"
	"testing"
	"time"
)

func TestSignV2KnownVector(t *testing.T) {
	nonce := strings.Repeat("00", 80)
	sig, err := SignV2(SignInput{
		Token:       "Bearer test-token",
		TimestampMS: "1700000000123",
		URL:         "accounts.goto-products.com/goto-auth/login/methods",
		Method:      "post",
		Body:        `{"a":1}`,
		Model:       "Xiaomi, MI 9",
		XM1:         "1:UNKNOWN,14:1700000000",
		OSInfo:      "Android,13",
		AppID:       DefaultAppID,
		Version:     DefaultAppVersion,
		UniqueID:    "0123456789abcdef",
		NonceHex:    nonce,
		PhoneMake:   "Google",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sig.BodyHash != "bb6cb5c68df4652941caf652a366f2d8" {
		t.Fatalf("body hash = %s", sig.BodyHash)
	}
	const wantHMAC = "17748b3acf6cf4e71366001671718de50ae731df37064a8f54e4d9780ff959d6"
	if sig.HMAC != wantHMAC {
		t.Fatalf("HMAC = %s", sig.HMAC)
	}
	if sig.XE1 != wantHMAC+":"+nonce+":D:1700000000123" {
		t.Fatalf("X-E1 = %s", sig.XE1)
	}
	if sig.XE2 != SignatureV2ID {
		t.Fatalf("X-E2 = %s", sig.XE2)
	}
}

func TestGenerateDeviceIdentityKnownVector(t *testing.T) {
	device := GenerateDeviceIdentity("+6281234567890")
	if device.Model != "vivo,V2310" || device.UniqueID != "62397bbd6a8c9ae5" || device.PhoneMake != "vivo" || device.OSInfo != "Android,14" {
		t.Fatalf("unexpected identity: %+v", device)
	}
	if device.SessionID != "4ddef781-a107-95fb-c295-9dbb9f551c6c" {
		t.Fatalf("session ID = %s", device.SessionID)
	}
	want := `1:UNKNOWN,2:UNKNOWN,3:1735404966747-66264351817589262,4:128000,5:mt6769|2000|8,6:3A:C9:14:A6:01:73,7:<unknown ssid>,8:720x1612,9:passive\,fused\,gps,10:0,11:HeuYNbmQJ/f6wr71rYALVMrtauqSJtzJSbJPTtDPx/g,12:VKEY_DISABLED,13:1003,14:1700000000,16:0,17:1`
	if got := device.xm1(time.Unix(1700000000, 0)); got != want {
		t.Fatalf("X-M1\n got: %s\nwant: %s", got, want)
	}
}
