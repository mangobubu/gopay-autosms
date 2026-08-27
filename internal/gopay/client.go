package gopay

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	proxyaddr "github.com/mangobubu/gopay-autosms/internal/proxy"
	"golang.org/x/net/proxy"
)

const (
	DefaultSSOBaseURL   = "https://accounts.goto-products.com"
	DefaultGoPayBaseURL = "https://customer.gopayapi.com"
	DefaultClientID     = "gopay:consumer:app"
	DefaultClientSecret = "raOUumeMRBNifqvZRFjvsgTnjAlaA9"
)

type Config struct {
	SSOBaseURL   string
	GoPayBaseURL string
	HTTPClient   *http.Client
	ProxyURL     string
	Timeout      time.Duration

	Phone      string
	DeviceSeed string
	Device     DeviceIdentity
	Session    *Session

	ClientID     string
	ClientSecret string
	AppBuild     string
	DeviceToken  string

	// Now and NonceReader make signatures reproducible in tests.
	Now         func() time.Time
	NonceReader io.Reader
	IDReader    io.Reader

	// The captured client sends /cvs/v1//initiate while signing the canonical
	// single-slash path. Set CVSInitiatePath to override it in a fixture.
	CVSInitiatePath string
}

type Client struct {
	mu sync.Mutex

	httpClient      *http.Client
	ssoBase         *url.URL
	gopayBase       *url.URL
	clientID        string
	secret          string
	appBuild        string
	deviceToken     string
	now             func() time.Time
	nonceReader     io.Reader
	idReader        io.Reader
	cvsInitiatePath string

	session Session
}

func NewClient(cfg Config) (*Client, error) {
	if cfg.SSOBaseURL == "" {
		cfg.SSOBaseURL = DefaultSSOBaseURL
	}
	if cfg.GoPayBaseURL == "" {
		cfg.GoPayBaseURL = DefaultGoPayBaseURL
	}
	ssoBase, err := url.Parse(strings.TrimRight(cfg.SSOBaseURL, "/"))
	if err != nil || ssoBase.Scheme == "" || ssoBase.Host == "" {
		return nil, fmt.Errorf("gopay: invalid SSO base URL %q", cfg.SSOBaseURL)
	}
	gopayBase, err := url.Parse(strings.TrimRight(cfg.GoPayBaseURL, "/"))
	if err != nil || gopayBase.Scheme == "" || gopayBase.Host == "" {
		return nil, fmt.Errorf("gopay: invalid GoPay base URL %q", cfg.GoPayBaseURL)
	}

	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.NonceReader == nil {
		cfg.NonceReader = rand.Reader
	}
	if cfg.IDReader == nil {
		cfg.IDReader = rand.Reader
	}
	if cfg.ClientID == "" {
		cfg.ClientID = DefaultClientID
	}
	if cfg.ClientSecret == "" {
		cfg.ClientSecret = DefaultClientSecret
	}
	if cfg.AppBuild == "" {
		cfg.AppBuild = DefaultAppBuild
	}
	if cfg.CVSInitiatePath == "" {
		cfg.CVSInitiatePath = "/cvs/v1//initiate"
	}

	var state Session
	if cfg.Session != nil {
		state = cloneSession(*cfg.Session)
	}
	if state.Phone == "" {
		state.Phone = cfg.Phone
	}
	if state.Device.UniqueID == "" {
		state.Device = cfg.Device
	}
	if strings.TrimSpace(state.ProxyURL) == "" {
		state.ProxyURL = cfg.ProxyURL
	}
	if strings.TrimSpace(state.ProxyURL) != "" {
		state.ProxyURL, err = normalizeProxyURL(state.ProxyURL)
		if err != nil {
			return nil, err
		}
	}
	httpClient, err := configuredHTTPClient(cfg.HTTPClient, state.ProxyURL, cfg.Timeout)
	if err != nil {
		return nil, err
	}
	if state.DeviceToken == "" {
		state.DeviceToken = cfg.DeviceToken
	}
	if state.Device.UniqueID == "" {
		seed := cfg.DeviceSeed
		if seed == "" {
			seed = state.Phone
		}
		state.Device = GenerateDeviceIdentity(seed)
	}
	state.Device = state.Device.withDefaults()
	if state.OTPLength == 0 {
		state.OTPLength = 4
	}
	if state.LoginStage == "" {
		state.LoginStage = LoginStageIdle
	}

	return &Client{
		httpClient:      httpClient,
		ssoBase:         ssoBase,
		gopayBase:       gopayBase,
		clientID:        cfg.ClientID,
		secret:          cfg.ClientSecret,
		appBuild:        cfg.AppBuild,
		deviceToken:     state.DeviceToken,
		now:             cfg.Now,
		nonceReader:     cfg.NonceReader,
		idReader:        cfg.IDReader,
		cvsInitiatePath: cfg.CVSInitiatePath,
		session:         state,
	}, nil
}

func NewClientForPhone(phone string, cfg Config) (*Client, error) {
	cfg.Phone = phone
	return NewClient(cfg)
}

func configuredHTTPClient(existing *http.Client, proxyRaw string, timeout time.Duration) (*http.Client, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	var client http.Client
	if existing != nil {
		client = *existing
	} else {
		client = http.Client{}
	}
	if client.Timeout == 0 {
		client.Timeout = timeout
	}
	if strings.TrimSpace(proxyRaw) != "" {
		normalized, err := normalizeProxyURL(proxyRaw)
		if err != nil {
			return nil, err
		}
		proxyURL, err := url.Parse(normalized)
		if err != nil {
			return nil, fmt.Errorf("gopay: invalid proxy URL %q", proxyRaw)
		}
		var transport *http.Transport
		switch t := client.Transport.(type) {
		case nil:
			transport = http.DefaultTransport.(*http.Transport).Clone()
		case *http.Transport:
			transport = t.Clone()
		default:
			return nil, fmt.Errorf("gopay: proxy requires an *http.Transport")
		}
		switch strings.ToLower(proxyURL.Scheme) {
		case "socks5", "socks5h":
			dialer, err := proxy.SOCKS5("tcp", proxyURL.Host, proxyAuth(proxyURL), proxy.Direct)
			if err != nil {
				return nil, fmt.Errorf("gopay: configure SOCKS5 proxy: %w", err)
			}
			contextDialer, ok := dialer.(proxy.ContextDialer)
			if !ok {
				return nil, fmt.Errorf("gopay: SOCKS5 proxy does not support context dialing")
			}
			transport.Proxy = nil
			transport.DialContext = contextDialer.DialContext
		case "http", "https":
			transport.Proxy = http.ProxyURL(proxyURL)
		default:
			return nil, fmt.Errorf("gopay: unsupported proxy scheme %q", proxyURL.Scheme)
		}
		client.Transport = transport
	}
	return &client, nil
}

func normalizeProxyURL(proxyRaw string) (string, error) {
	raw := strings.TrimSpace(strings.ReplaceAll(proxyRaw, `\@`, "@"))
	if raw == "" {
		return "", nil
	}
	normalized, err := proxyaddr.Normalize(raw)
	if err != nil {
		return "", fmt.Errorf("gopay: invalid proxy URL %q", proxyRaw)
	}
	return normalized, nil
}

func proxyAuth(proxyURL *url.URL) *proxy.Auth {
	if proxyURL.User == nil {
		return nil
	}
	password, _ := proxyURL.User.Password()
	return &proxy.Auth{User: proxyURL.User.Username(), Password: password}
}

func (c *Client) State() Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	return cloneSession(c.session)
}

func (c *Client) Restore(state Session) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state.Device = state.Device.withDefaults()
	c.session = cloneSession(state)
}

func cloneSession(s Session) Session {
	s.Methods = append([]string(nil), s.Methods...)
	s.TwoFAMethods = append([]string(nil), s.TwoFAMethods...)
	return s
}

type apiResponse struct {
	status int
	body   []byte
	json   map[string]any
}

func (c *Client) ssoPost(ctx context.Context, path, canonicalPath string, body any, extra http.Header) (apiResponse, error) {
	if canonicalPath == "" {
		canonicalPath = path
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return apiResponse{}, fmt.Errorf("gopay: encode request: %w", err)
	}
	headers, err := c.ssoHeaders(canonicalPath, encoded)
	if err != nil {
		return apiResponse{}, err
	}
	mergeHeaders(headers, extra)
	return c.do(ctx, c.ssoBase.String()+path, http.MethodPost, encoded, headers)
}

func (c *Client) gopayRequest(ctx context.Context, method, path string, body any, extra http.Header) (apiResponse, error) {
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			return apiResponse{}, fmt.Errorf("gopay: encode request: %w", err)
		}
	}
	headers, err := c.gopayHeaders(path, method, encoded)
	if err != nil {
		return apiResponse{}, err
	}
	mergeHeaders(headers, extra)
	return c.do(ctx, c.gopayBase.String()+path, method, encoded, headers)
}

func (c *Client) do(ctx context.Context, rawURL, method string, body []byte, headers http.Header) (apiResponse, error) {
	request, err := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(body))
	if err != nil {
		return apiResponse{}, fmt.Errorf("gopay: create request: %w", err)
	}
	request.Header = headers.Clone()
	response, err := c.httpClient.Do(request)
	if err != nil {
		return apiResponse{}, fmt.Errorf("gopay: %s %s: %w", method, request.URL.Path, err)
	}
	defer response.Body.Close()
	var responseReader io.Reader = response.Body
	if strings.EqualFold(strings.TrimSpace(response.Header.Get("Content-Encoding")), "gzip") {
		gzipReader, gzipErr := gzip.NewReader(response.Body)
		if gzipErr != nil {
			return apiResponse{}, fmt.Errorf("gopay: decode gzip response: %w", gzipErr)
		}
		defer gzipReader.Close()
		responseReader = gzipReader
	}
	data, err := io.ReadAll(io.LimitReader(responseReader, 2<<20))
	if err != nil {
		return apiResponse{}, fmt.Errorf("gopay: read response: %w", err)
	}
	result := apiResponse{status: response.StatusCode, body: data, json: map[string]any{}}
	if len(data) != 0 {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		if err := decoder.Decode(&result.json); err != nil && response.StatusCode >= 200 && response.StatusCode < 300 {
			return result, fmt.Errorf("gopay: decode HTTP %d response: %w", response.StatusCode, err)
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		bounded := append([]byte(nil), data...)
		if len(bounded) > 4096 {
			bounded = bounded[:4096]
		}
		return result, &HTTPError{StatusCode: response.StatusCode, Body: bounded}
	}
	return result, nil
}

func (c *Client) ssoHeaders(path string, body []byte) (http.Header, error) {
	device := c.session.Device
	now := c.now()
	xm1 := device.xm1(now)
	sig, err := c.sign(c.ssoBase.Host+path, http.MethodPost, body, xm1, now)
	if err != nil {
		return nil, err
	}
	h := c.baseHeaders()
	h.Set("X-M1", xm1)
	h.Set("X-CVSDK-Version", "1.0.0")
	h.Set("X-E1", sig.XE1)
	h.Set("X-E2", sig.XE2)
	h.Set("X-Request-ID", c.newID())
	h.Set("Gojek-Country-Code", "ID")
	h.Set("Country-Code", "ID")
	h.Set("Gojek-Service-Area", "1")
	h.Set("Gojek-Timezone", "Asia/Jakarta")
	h.Set("Accept-Encoding", "gzip")
	return h, nil
}

func (c *Client) gopayHeaders(path, method string, body []byte) (http.Header, error) {
	device := c.session.Device
	now := c.now()
	xm1 := device.xm1(now)
	sig, err := c.sign(c.gopayBase.Host+path, method, body, xm1, now)
	if err != nil {
		return nil, err
	}
	h := c.baseHeaders()
	h.Set("Content-Type", "application/json")
	h.Set("Accept-Language", "id-ID")
	h.Set("X-User-Locale", "id_ID")
	h.Set("X-M1", xm1)
	h.Set("X-E1", sig.XE1)
	h.Set("X-E2", sig.XE2)
	h.Set("User-uuid", c.session.AccountID)
	h.Set("X-PushTokenType", "FCM")
	h.Set("X-Location", "-6.2088,106.8456")
	h.Set("X-Location-Accuracy", "5.0")
	h.Set("Gojek-Country-Code", "ID")
	h.Set("Country-Code", "ID")
	h.Set("Gojek-Service-Area", "1")
	h.Set("Gojek-Timezone", "Asia/Jakarta")
	h.Set("X-Dark-Mode", "false")
	h.Set("support-sdk-version", "0.49.1")
	// Explicit gzip keeps response handling deterministic; advertising Brotli
	// would disable Go's automatic decoding without a Brotli decoder here.
	h.Set("Accept-Encoding", "gzip")
	return h, nil
}

func (c *Client) baseHeaders() http.Header {
	d := c.session.Device
	h := make(http.Header)
	h.Set("Content-Type", "application/json; charset=UTF-8")
	h.Set("Accept", "application/json")
	h.Set("X-AppVersion", d.Version)
	h.Set("X-AppId", d.AppID)
	h.Set("X-AppType", "GOPAY")
	h.Set("X-UniqueId", d.UniqueID)
	h.Set("X-Platform", "Android")
	h.Set("X-DeviceOS", d.OSInfo)
	h.Set("X-DeviceToken", c.deviceToken)
	h.Set("X-PhoneMake", d.PhoneMake)
	h.Set("X-PhoneModel", d.Model)
	h.Set("X-User-Type", "customer")
	h.Set("X-User-Locale", "en_ID")
	h.Set("X-Help-Version", d.Version)
	h.Set("X-AuthSDK-Version", "1.0.0")
	h.Set("Transaction-ID", c.session.TransactionID)
	h.Set("User-Agent", fmt.Sprintf("GoPay/%s (%s; build:%s; %s)", d.Version, d.AppID, c.appBuild, d.OSInfo))
	h.Set("Accept-Language", "en-ID")
	if c.session.AccessToken != "" {
		h.Set("Authorization", bearer(c.session.AccessToken))
	}
	return h
}

func (c *Client) sign(target, method string, body []byte, xm1 string, now time.Time) (Signature, error) {
	nonce := make([]byte, 80)
	if _, err := io.ReadFull(c.nonceReader, nonce); err != nil {
		return Signature{}, fmt.Errorf("gopay: generate signing nonce: %w", err)
	}
	return SignV2(SignInput{
		Token: c.session.AccessToken, TimestampMS: fmt.Sprintf("%d", now.UnixMilli()),
		URL: target, Method: method, Body: string(body), Model: c.session.Device.Model,
		XM1: xm1, OSInfo: c.session.Device.OSInfo, AppID: c.session.Device.AppID,
		Version: c.session.Device.Version, UniqueID: c.session.Device.UniqueID,
		NonceHex: hex.EncodeToString(nonce), PhoneMake: c.session.Device.PhoneMake,
	})
}

func mergeHeaders(dst, src http.Header) {
	for key, values := range src {
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func bearer(token string) string {
	if token == "" || strings.HasPrefix(token, "Bearer ") {
		return token
	}
	return "Bearer " + token
}

func (c *Client) newID() string {
	b := make([]byte, 16)
	if _, err := io.ReadFull(c.idReader, b); err != nil {
		return fmt.Sprintf("%d", c.now().UnixNano())
	}
	// UUIDv4 layout is used only as a request/transaction correlation value.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return formatUUID(b)
}

func dataObject(body map[string]any) map[string]any {
	if data, ok := body["data"].(map[string]any); ok {
		return data
	}
	return body
}

func stringsFrom(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func stringValue(obj map[string]any, key string) string {
	value, _ := obj[key].(string)
	return value
}
