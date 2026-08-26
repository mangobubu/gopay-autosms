package gopay

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	DefaultAppID      = "com.gojek.gopay"
	DefaultAppVersion = "2.8.0"
	DefaultAppBuild   = "2080"
	OriginalD1        = "CF:43:60:94:46:9C:A0:8F:CB:5C:95:05:97:E9:03:51:40:0A:C7:33:EC:BA:40:71:F1:94:DC:CE:BA:AE:4C:A8"
)

type deviceProfile struct {
	brand, manufacturer, model, platform string
	cpuMHz, cpuCores                     int
	screen                               string
	androidVersions                      []string
	diskMB                               []int
}

var deviceProfiles = []deviceProfile{
	{"samsung", "samsung", "SM-A546E", "exynos1380", 2400, 8, "1080x2340", []string{"13", "14"}, []int{128000, 131072, 262144}},
	{"samsung", "samsung", "SM-A536E", "exynos1280", 2400, 8, "1080x2400", []string{"12", "13", "14"}, []int{128000, 131072}},
	{"samsung", "samsung", "SM-A346E", "mt6877", 2600, 8, "1080x2340", []string{"13", "14"}, []int{128000, 131072}},
	{"samsung", "samsung", "SM-A256E", "exynos1280", 2400, 8, "1080x2340", []string{"14"}, []int{128000, 131072}},
	{"samsung", "samsung", "SM-M336BU", "exynos1280", 2400, 8, "1080x2408", []string{"12", "13"}, []int{128000}},
	{"Xiaomi", "Xiaomi", "2201117TY", "taro", 3000, 8, "1080x2400", []string{"12", "13"}, []int{128000, 256000}},
	{"Xiaomi", "Xiaomi", "23053RN02A", "mt6768", 2000, 8, "1080x2400", []string{"13"}, []int{128000}},
	{"Xiaomi", "Xiaomi", "2312DRA50G", "garnet", 2800, 8, "1220x2712", []string{"14"}, []int{256000, 262144}},
	{"Redmi", "Xiaomi", "2209116AG", "mt8781", 2200, 8, "1080x2400", []string{"12", "13"}, []int{128000}},
	{"Redmi", "Xiaomi", "23090RA98G", "mt6789", 2200, 8, "1080x2460", []string{"13", "14"}, []int{128000, 256000}},
	{"POCO", "Xiaomi", "23049PCD8G", "mt6833", 2200, 8, "1080x2400", []string{"13", "14"}, []int{128000, 256000}},
	{"POCO", "Xiaomi", "22101320G", "taro", 3200, 8, "1080x2400", []string{"12", "13", "14"}, []int{256000}},
	{"OPPO", "OPPO", "CPH2565", "mt6833", 2200, 8, "720x1612", []string{"13"}, []int{128000}},
	{"OPPO", "OPPO", "CPH2387", "mt6833", 2200, 8, "1080x2400", []string{"12", "13"}, []int{128000}},
	{"OPPO", "OPPO", "CPH2529", "mt6769", 2000, 8, "720x1612", []string{"13"}, []int{128000}},
	{"vivo", "vivo", "V2248", "mt6769", 2000, 8, "720x1612", []string{"13"}, []int{128000}},
	{"vivo", "vivo", "V2204", "mt6833", 2200, 8, "1080x2404", []string{"12", "13"}, []int{128000}},
	{"vivo", "vivo", "V2310", "mt6769", 2000, 8, "720x1612", []string{"13", "14"}, []int{128000}},
	{"realme", "realme", "RMX3710", "mt6833", 2200, 8, "1080x2400", []string{"13"}, []int{128000, 256000}},
	{"realme", "realme", "RMX3630", "mt6833", 2200, 8, "1080x2408", []string{"12", "13"}, []int{128000}},
	{"realme", "realme", "RMX3830", "mt6769", 2000, 8, "720x1604", []string{"13", "14"}, []int{128000}},
	{"Infinix", "INFINIX", "X6833B", "mt6789", 2200, 8, "1080x2460", []string{"13"}, []int{128000, 256000}},
	{"Infinix", "INFINIX", "X6711", "mt6769", 2000, 8, "1080x2460", []string{"12", "13"}, []int{128000}},
	{"TECNO", "TECNO", "CK8n", "mt6769", 2000, 8, "720x1612", []string{"13"}, []int{128000}},
}

// DeviceIdentity is stable for a seed and is safe to persist as JSON.
type DeviceIdentity struct {
	D1          string `json:"d1"`
	Model       string `json:"model"`
	UniqueID    string `json:"unique_id"`
	XM1Template string `json:"xm1_template"`
	PhoneMake   string `json:"phone_make"`
	OSInfo      string `json:"os_info"`
	AppID       string `json:"app_id"`
	Version     string `json:"version"`
	SessionID   string `json:"session_id"`
}

var nonDigits = regexp.MustCompile(`\D+`)

// GenerateDeviceIdentity ports the attachment's stable Android identity
// derivation. The changing X-M1 timestamp is added only when a request is sent.
func GenerateDeviceIdentity(seed string) DeviceIdentity {
	if seed == "" {
		seed = "gopay-device"
	}
	normalized := nonDigits.ReplaceAllString(seed, "")
	if normalized == "" {
		normalized = seed
	}
	h := sha256.Sum256([]byte(seed))
	p := deviceProfiles[int(binary.BigEndian.Uint16(h[0:2]))%len(deviceProfiles)]

	androidID := fmt.Sprintf("%x", h[:8])
	drm := sha256.Sum256(append([]byte("widevine:"), []byte(normalized)...))
	drmID := base64.RawStdEncoding.EncodeToString(drm[:])
	macBytes := append([]byte(nil), h[8:14]...)
	macBytes[0] = (macBytes[0] | 0x02) & 0xfe
	mac := fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X", macBytes[0], macBytes[1], macBytes[2], macBytes[3], macBytes[4], macBytes[5])

	installOffset := uint64(0)
	for _, b := range h[14:20] {
		installOffset = installOffset<<8 | uint64(b)
	}
	const installWindowMS = uint64(630 * 24 * 60 * 60 * 1000)
	baseMS := uint64(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()) + installOffset%installWindowMS
	installRandom := binary.BigEndian.Uint64(h[14:22])
	disk := p.diskMB[int(binary.BigEndian.Uint16(h[22:24]))%len(p.diskMB)]
	android := p.androidVersions[int(binary.BigEndian.Uint16(h[24:26]))%len(p.androidVersions)]
	sessionHash := sha256.Sum256(append([]byte("session:"), []byte(normalized)...))
	sessionID := formatUUID(sessionHash[:16])

	xm1 := fmt.Sprintf("1:UNKNOWN,2:UNKNOWN,3:%d-%d,4:%d,5:%s|%d|%d,6:%s,7:<unknown ssid>,8:%s,9:passive\\,fused\\,gps,10:0,11:%s,12:VKEY_DISABLED,13:1003,14:%d,16:0,17:1",
		baseMS, installRandom, disk, p.platform, p.cpuMHz, p.cpuCores, mac, p.screen, drmID, baseMS/1000)

	return DeviceIdentity{
		D1:          OriginalD1,
		Model:       p.brand + "," + p.model,
		UniqueID:    androidID,
		XM1Template: xm1,
		PhoneMake:   p.manufacturer,
		OSInfo:      "Android," + android,
		AppID:       DefaultAppID,
		Version:     DefaultAppVersion,
		SessionID:   sessionID,
	}
}

func (d DeviceIdentity) withDefaults() DeviceIdentity {
	if d.D1 == "" {
		d.D1 = OriginalD1
	}
	if d.Model == "" {
		d.Model = "Xiaomi,MI 9"
	}
	if d.PhoneMake == "" {
		d.PhoneMake = "Google"
	}
	if d.OSInfo == "" {
		d.OSInfo = "Android,13"
	}
	if d.AppID == "" {
		d.AppID = DefaultAppID
	}
	if d.Version == "" {
		d.Version = DefaultAppVersion
	}
	return d
}

func (d DeviceIdentity) xm1(now time.Time) string {
	return regexp.MustCompile(`14:\d+`).ReplaceAllString(d.XM1Template, fmt.Sprintf("14:%d", now.Unix()))
}

func formatUUID(b []byte) string {
	h := fmt.Sprintf("%x", b)
	if len(h) != 32 {
		return strings.TrimSpace(h)
	}
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
