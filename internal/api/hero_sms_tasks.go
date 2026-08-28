package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/mangobubu/gopay-autosms/internal/domain"
	"github.com/mangobubu/gopay-autosms/internal/herosms"
	"github.com/mangobubu/gopay-autosms/internal/herotask"
	appsettings "github.com/mangobubu/gopay-autosms/internal/settings"
	"github.com/mangobubu/gopay-autosms/internal/smsbower"
	"github.com/mangobubu/gopay-autosms/internal/storage"
)

const (
	heroSMSDefaultRefundWindow = 20 * time.Minute
	heroSMSMaxIdempotencyKey   = 128
)

// heroSMSCatalogClient contains the provider operations needed by the
// independent number-purchase page. Keeping the transport behind this small
// boundary makes the HTTP contract testable without provider network calls.
type heroSMSCatalogClient interface {
	GetServicesList(context.Context) ([]smsbower.Service, error)
	GetCountries(context.Context) ([]smsbower.Country, error)
	RentAvailability(context.Context, herosms.RentAvailabilityRequest) ([]herosms.Offer, error)
	Offers(context.Context, herosms.OfferRequest) ([]herosms.Offer, error)
}

// HeroSMSTaskController is implemented by the independent task manager. Each
// method operates on exactly one number task; there is intentionally no pool
// or batch action at this API boundary.
type HeroSMSTaskController interface {
	CreateTasks(context.Context, HeroSMSCreateTasksInput) ([]domain.HeroSMSNumberTask, error)
	ListTasks(context.Context, storage.Page) ([]domain.HeroSMSNumberTask, error)
	GetTask(context.Context, int64) (domain.HeroSMSNumberTask, error)
	StartTask(context.Context, int64) (domain.HeroSMSNumberTask, error)
	StopTask(context.Context, int64) (domain.HeroSMSNumberTask, error)
}

// HeroSMSCreateTasksInput is the stable HTTP-to-manager boundary. The manager
// may derive provider purchase details, but the caller's idempotency key is
// always preserved as SubmissionID.
type HeroSMSCreateTasksInput struct {
	SubmissionID     string
	ProductKind      domain.HeroSMSProductKind
	ServiceCode      string
	ServiceName      string
	CountryCode      string
	CountryName      string
	VerificationType string
	DurationHours    *int
	MaxPriceAmount   string
	Currency         string
	Operator         string
	Quantity         int
}

type heroSMSTaskManagerAdapter struct {
	manager *herotask.Manager
}

// AdaptHeroSMSTaskManager converts the concrete scheduler manager to the small
// API-facing controller. It keeps herotask independent from the HTTP package.
func AdaptHeroSMSTaskManager(manager *herotask.Manager) HeroSMSTaskController {
	if manager == nil {
		return nil
	}
	return &heroSMSTaskManagerAdapter{manager: manager}
}

func (adapter *heroSMSTaskManagerAdapter) CreateTasks(ctx context.Context, input HeroSMSCreateTasksInput) ([]domain.HeroSMSNumberTask, error) {
	return adapter.manager.CreateTasks(ctx, herotask.CreateTasksInput{
		SubmissionID: input.SubmissionID, ProductKind: input.ProductKind,
		ServiceCode: input.ServiceCode, ServiceName: input.ServiceName,
		CountryCode: input.CountryCode, CountryName: input.CountryName,
		VerificationType: herosms.VerificationType(input.VerificationType),
		DurationHours:    cloneInt(input.DurationHours), MaxPrice: input.MaxPriceAmount,
		Currency: input.Currency, Operator: input.Operator, Quantity: input.Quantity,
	})
}

func (adapter *heroSMSTaskManagerAdapter) ListTasks(ctx context.Context, page storage.Page) ([]domain.HeroSMSNumberTask, error) {
	return adapter.manager.ListTasks(ctx, storage.HeroSMSTaskFilter{Page: page})
}

func (adapter *heroSMSTaskManagerAdapter) GetTask(ctx context.Context, id int64) (domain.HeroSMSNumberTask, error) {
	return adapter.manager.GetTask(ctx, id)
}

func (adapter *heroSMSTaskManagerAdapter) StartTask(ctx context.Context, id int64) (domain.HeroSMSNumberTask, error) {
	return adapter.manager.StartTask(ctx, id)
}

func (adapter *heroSMSTaskManagerAdapter) StopTask(ctx context.Context, id int64) (domain.HeroSMSNumberTask, error) {
	return adapter.manager.StopTask(ctx, id)
}

// HeroSMSAPI owns the routes for the standalone HeroSMS purchase page. It is
// separate from the legacy batch workflow so adding a new number never stops,
// replaces, or consumes capacity from an existing task.
type HeroSMSAPI struct {
	settings  *appsettings.Manager
	tasks     HeroSMSTaskController
	newClient func(context.Context) (heroSMSCatalogClient, error)
	now       func() time.Time
}

func NewHeroSMSAPI(settings *appsettings.Manager, tasks HeroSMSTaskController) *HeroSMSAPI {
	handler := &HeroSMSAPI{settings: settings, tasks: tasks, now: time.Now}
	handler.newClient = handler.newHeroSMSClient
	return handler
}

// RegisterHeroSMSRoutes registers both the catalogue and task lifecycle under
// whichever API prefix owns group (normally /api and /api/v1).
func (h *HeroSMSAPI) RegisterHeroSMSRoutes(group *gin.RouterGroup) {
	group.GET("/hero-sms/catalog", h.getCatalog)
	group.POST("/hero-sms/tasks", h.createTasks)
	group.GET("/hero-sms/tasks", h.listTasks)
	group.POST("/hero-sms/tasks/:id/start", h.startTask)
	group.POST("/hero-sms/tasks/:id/stop", h.stopTask)
	group.POST("/hero-sms/tasks/:id/cancel", h.cancelTask)
	group.POST("/hero-sms/tasks/:id/settle", h.settleTask)
}

func (h *HeroSMSAPI) newHeroSMSClient(ctx context.Context) (heroSMSCatalogClient, error) {
	if h == nil || h.settings == nil {
		return nil, errors.New("HeroSMS settings are unavailable")
	}
	value, err := h.settings.GetHeroSMS(ctx)
	if err != nil {
		return nil, err
	}
	return herosms.NewClient(herosms.Config{APIKey: value.APIKey, BaseURL: value.BaseURL})
}

type heroSMSCatalogQuery struct {
	Service          string
	Country          *int
	VerificationType herosms.VerificationType
	DurationHours    *int
}

type heroSMSCatalogVerificationType struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type heroSMSCatalogDuration struct {
	DurationHours int    `json:"duration_hours"`
	Label         string `json:"label"`
}

type heroSMSCatalogOffer struct {
	Service                 string                    `json:"service"`
	Country                 int                       `json:"country"`
	ProductKind             domain.HeroSMSProductKind `json:"product_kind"`
	VerificationType        herosms.VerificationType  `json:"verification_type"`
	DurationHours           *int                      `json:"duration_hours,omitempty"`
	Price                   string                    `json:"price,omitempty"`
	RetailPrice             string                    `json:"retail_price,omitempty"`
	Currency                string                    `json:"currency,omitempty"`
	Stock                   int                       `json:"stock"`
	Available               bool                      `json:"available"`
	PhysicalCount           int                       `json:"physical_count,omitempty"`
	DefaultPriceCount       int                       `json:"default_price_count,omitempty"`
	PriceCounts             map[string]int            `json:"price_counts,omitempty"`
	Operators               []string                  `json:"operators,omitempty"`
	RefundableWindowSeconds int                       `json:"refundable_window_seconds,omitempty"`
}

type heroSMSCatalogResponse struct {
	Services          []smsbower.Service               `json:"services"`
	Countries         []smsbower.Country               `json:"countries"`
	VerificationTypes []heroSMSCatalogVerificationType `json:"verification_types"`
	Durations         []heroSMSCatalogDuration         `json:"durations"`
	Offers            []heroSMSCatalogOffer            `json:"offers"`
	Message           string                           `json:"message,omitempty"`
}

func (h *HeroSMSAPI) getCatalog(c *gin.Context) {
	query, err := parseHeroSMSCatalogQuery(c)
	if err != nil {
		respondError(c, err)
		return
	}
	client, err := h.newClient(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}

	services, err := client.GetServicesList(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	countries, err := client.GetCountries(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	response := heroSMSCatalogResponse{
		Services:  nonNilServices(services),
		Countries: nonNilCountries(countries),
		Offers:    []heroSMSCatalogOffer{},
		Durations: []heroSMSCatalogDuration{},
	}
	if query.Service == "" {
		response.VerificationTypes = []heroSMSCatalogVerificationType{}
		c.JSON(http.StatusOK, response)
		return
	}

	response.VerificationTypes = []heroSMSCatalogVerificationType{
		{Value: string(herosms.VerificationSMS), Label: "短信验证码"},
		{Value: string(herosms.VerificationCall), Label: "语音验证码"},
	}
	if query.VerificationType != herosms.VerificationCall {
		rentOffers, rentErr := client.RentAvailability(c.Request.Context(), herosms.RentAvailabilityRequest{Service: query.Service})
		if rentErr != nil {
			if !herosms.IsNoNumbers(rentErr) && !smsbower.IsAPIError(rentErr, "BAD_SERVICE") {
				respondError(c, rentErr)
				return
			}
			// BAD_SERVICE from serviceCountRent means this service has no rental
			// product. It does not imply that ordinary activation offers are
			// unavailable, so continue with an empty duration catalogue.
			rentOffers = nil
		}
		response.Offers = append(response.Offers, catalogOffers(rentOffers)...)
		response.Durations = catalogDurations(rentOffers)
	}

	if query.Country == nil {
		c.JSON(http.StatusOK, response)
		return
	}

	exactRequests := exactHeroSMSOfferRequests(query)
	exactOffers := make([]herosms.Offer, 0)
	for _, request := range exactRequests {
		items, offerErr := client.Offers(c.Request.Context(), request)
		if offerErr != nil && !herosms.IsNoNumbers(offerErr) {
			respondError(c, offerErr)
			return
		}
		if len(items) == 0 {
			items = []herosms.Offer{emptyHeroSMSOffer(request)}
		}
		exactOffers = append(exactOffers, items...)
	}
	response.Offers = mergeCatalogOffers(response.Offers, catalogOffers(exactOffers))
	if !heroSMSCatalogHasAvailableOffer(response.Offers, query) {
		response.Message = "该服务暂无可用号码；创建任务后会持续轮询购买"
	}
	c.JSON(http.StatusOK, response)
}

func parseHeroSMSCatalogQuery(c *gin.Context) (heroSMSCatalogQuery, error) {
	query := heroSMSCatalogQuery{Service: strings.TrimSpace(c.Query("service"))}
	countryText := strings.TrimSpace(c.Query("country"))
	verificationText := strings.ToLower(strings.TrimSpace(c.Query("verification_type")))
	durationText := strings.TrimSpace(c.Query("duration_hours"))

	if countryText != "" {
		country, err := strconv.Atoi(countryText)
		if err != nil || country < 0 || country > 999 {
			return query, fmt.Errorf("%w: country must be an integer between 0 and 999", storage.ErrInvalidInput)
		}
		query.Country = &country
	}
	if verificationText != "" {
		query.VerificationType = herosms.VerificationType(verificationText)
		if query.VerificationType != herosms.VerificationSMS && query.VerificationType != herosms.VerificationCall {
			return query, fmt.Errorf("%w: verification_type must be sms or call", storage.ErrInvalidInput)
		}
	}
	if durationText != "" {
		duration, err := strconv.Atoi(durationText)
		if err != nil || duration < 0 {
			return query, fmt.Errorf("%w: duration_hours must be a non-negative integer", storage.ErrInvalidInput)
		}
		query.DurationHours = &duration
	}
	if query.Service == "" && (query.Country != nil || query.VerificationType != "" || query.DurationHours != nil) {
		return query, fmt.Errorf("%w: service is required before dependent catalogue filters", storage.ErrInvalidInput)
	}
	if query.Country == nil && (query.VerificationType != "" || query.DurationHours != nil) {
		return query, fmt.Errorf("%w: country is required before verification_type or duration_hours", storage.ErrInvalidInput)
	}
	if query.DurationHours != nil && *query.DurationHours > 0 && query.VerificationType == herosms.VerificationCall {
		return query, fmt.Errorf("%w: rental durations support sms verification only", storage.ErrInvalidInput)
	}
	return query, nil
}

func exactHeroSMSOfferRequests(query heroSMSCatalogQuery) []herosms.OfferRequest {
	duration := 0
	if query.DurationHours != nil {
		duration = *query.DurationHours
	}
	base := herosms.OfferRequest{Service: query.Service, Country: *query.Country, DurationHours: duration}
	if duration > 0 {
		base.VerificationType = herosms.VerificationSMS
		return []herosms.OfferRequest{base}
	}
	if query.VerificationType != "" {
		base.VerificationType = query.VerificationType
		return []herosms.OfferRequest{base}
	}
	sms := base
	sms.VerificationType = herosms.VerificationSMS
	call := base
	call.VerificationType = herosms.VerificationCall
	return []herosms.OfferRequest{sms, call}
}

func emptyHeroSMSOffer(request herosms.OfferRequest) herosms.Offer {
	return herosms.Offer{
		Service: request.Service, Country: request.Country, DurationHours: request.DurationHours,
		VerificationType: request.VerificationType, Count: 0,
	}
}

func catalogOffers(offers []herosms.Offer) []heroSMSCatalogOffer {
	result := make([]heroSMSCatalogOffer, 0, len(offers))
	for _, offer := range offers {
		kind := domain.HeroSMSProductActivation
		var duration *int
		refundSeconds := int(heroSMSDefaultRefundWindow / time.Second)
		if offer.DurationHours > 0 {
			kind = domain.HeroSMSProductRent
			value := offer.DurationHours
			duration = &value
		}
		verificationType := offer.VerificationType
		if verificationType == "" {
			verificationType = herosms.VerificationSMS
		}
		result = append(result, heroSMSCatalogOffer{
			Service: offer.Service, Country: offer.Country, ProductKind: kind,
			VerificationType: verificationType, DurationHours: duration,
			Price: offer.Price, RetailPrice: offer.RetailPrice, Currency: offer.Currency,
			Stock: offer.Count, Available: offer.Count > 0, PhysicalCount: offer.PhysicalCount,
			DefaultPriceCount: offer.DefaultPriceCount, PriceCounts: offer.PriceCounts,
			Operators: offer.Operators, RefundableWindowSeconds: refundSeconds,
		})
	}
	return result
}

func catalogDurations(offers []herosms.Offer) []heroSMSCatalogDuration {
	seen := make(map[int]struct{})
	result := make([]heroSMSCatalogDuration, 0)
	for _, offer := range offers {
		if offer.DurationHours <= 0 {
			continue
		}
		if _, exists := seen[offer.DurationHours]; exists {
			continue
		}
		seen[offer.DurationHours] = struct{}{}
		result = append(result, heroSMSCatalogDuration{
			DurationHours: offer.DurationHours,
			Label:         heroSMSDurationLabel(offer.DurationHours),
		})
	}
	return result
}

func heroSMSDurationLabel(hours int) string {
	if hours > 0 && hours%24 == 0 {
		return fmt.Sprintf("%d 天", hours/24)
	}
	return fmt.Sprintf("%d 小时", hours)
}

func mergeCatalogOffers(base, exact []heroSMSCatalogOffer) []heroSMSCatalogOffer {
	indexes := make(map[string]int, len(base)+len(exact))
	result := append([]heroSMSCatalogOffer(nil), base...)
	for index := range result {
		indexes[heroSMSCatalogOfferKey(result[index])] = index
	}
	for _, offer := range exact {
		key := heroSMSCatalogOfferKey(offer)
		if index, exists := indexes[key]; exists {
			result[index] = offer
			continue
		}
		indexes[key] = len(result)
		result = append(result, offer)
	}
	return result
}

func heroSMSCatalogOfferKey(offer heroSMSCatalogOffer) string {
	duration := 0
	if offer.DurationHours != nil {
		duration = *offer.DurationHours
	}
	return fmt.Sprintf("%s:%d:%s:%d", offer.Service, offer.Country, offer.VerificationType, duration)
}

func heroSMSCatalogHasAvailableOffer(offers []heroSMSCatalogOffer, query heroSMSCatalogQuery) bool {
	for _, offer := range offers {
		if !offer.Available || offer.Service != query.Service {
			continue
		}
		if query.Country != nil && offer.Country != *query.Country {
			continue
		}
		if query.VerificationType != "" && offer.VerificationType != query.VerificationType {
			continue
		}
		if query.DurationHours != nil {
			duration := 0
			if offer.DurationHours != nil {
				duration = *offer.DurationHours
			}
			if duration != *query.DurationHours {
				continue
			}
		}
		return true
	}
	return false
}

func nonNilServices(items []smsbower.Service) []smsbower.Service {
	if items == nil {
		return []smsbower.Service{}
	}
	return items
}

func nonNilCountries(items []smsbower.Country) []smsbower.Country {
	if items == nil {
		return []smsbower.Country{}
	}
	return items
}

type createHeroSMSTasksRequest struct {
	Service          string                   `json:"service"`
	Country          string                   `json:"country"`
	VerificationType herosms.VerificationType `json:"verification_type"`
	DurationHours    *int                     `json:"duration_hours"`
	Quantity         int                      `json:"quantity"`
}

type heroSMSTaskCapabilities struct {
	Start  bool `json:"start"`
	Stop   bool `json:"stop"`
	Cancel bool `json:"cancel"`
	Settle bool `json:"settle"`
}

type heroSMSTaskView struct {
	domain.HeroSMSNumberTask
	RequestedDurationSeconds *int64                  `json:"requested_duration_seconds,omitempty"`
	EffectiveDurationSeconds *int64                  `json:"effective_duration_seconds,omitempty"`
	Refundable               bool                    `json:"refundable"`
	Running                  bool                    `json:"running"`
	Capabilities             heroSMSTaskCapabilities `json:"capabilities"`
}

type heroSMSTasksResponse struct {
	Tasks      []heroSMSTaskView `json:"tasks"`
	ServerNow  time.Time         `json:"server_now"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

func (h *HeroSMSAPI) createTasks(c *gin.Context) {
	if h == nil || h.tasks == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "HeroSMS task manager is unavailable"})
		return
	}
	submissionID, err := parseHeroSMSIdempotencyKey(c.GetHeader("Idempotency-Key"))
	if err != nil {
		respondError(c, err)
		return
	}
	var request createHeroSMSTasksRequest
	if err = c.ShouldBindJSON(&request); err != nil {
		respondError(c, fmt.Errorf("%w: invalid HeroSMS task request: %v", storage.ErrInvalidInput, err))
		return
	}
	input, err := request.input(submissionID)
	if err != nil {
		respondError(c, err)
		return
	}
	input, err = h.enrichCreateInput(c.Request.Context(), input)
	if err != nil {
		respondError(c, err)
		return
	}
	tasks, err := h.tasks.CreateTasks(c.Request.Context(), input)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusCreated, h.tasksResponse(tasks))
}

func (request createHeroSMSTasksRequest) input(submissionID string) (HeroSMSCreateTasksInput, error) {
	request.Service = strings.TrimSpace(request.Service)
	request.Country = strings.TrimSpace(request.Country)
	request.VerificationType = herosms.VerificationType(strings.ToLower(strings.TrimSpace(string(request.VerificationType))))
	switch {
	case request.Service == "":
		return HeroSMSCreateTasksInput{}, fmt.Errorf("%w: service is required", storage.ErrInvalidInput)
	case len(request.Service) > 64:
		return HeroSMSCreateTasksInput{}, fmt.Errorf("%w: service is too long", storage.ErrInvalidInput)
	case request.Country == "":
		return HeroSMSCreateTasksInput{}, fmt.Errorf("%w: country is required", storage.ErrInvalidInput)
	case request.VerificationType != herosms.VerificationSMS && request.VerificationType != herosms.VerificationCall:
		return HeroSMSCreateTasksInput{}, fmt.Errorf("%w: verification_type must be sms or call", storage.ErrInvalidInput)
	case request.Quantity < 1 || request.Quantity > storage.MaxHeroSMSTaskQuantity:
		return HeroSMSCreateTasksInput{}, fmt.Errorf("%w: quantity must be between 1 and %d", storage.ErrInvalidInput, storage.MaxHeroSMSTaskQuantity)
	case request.DurationHours != nil && *request.DurationHours < 0:
		return HeroSMSCreateTasksInput{}, fmt.Errorf("%w: duration_hours must be a non-negative integer", storage.ErrInvalidInput)
	case request.DurationHours != nil && *request.DurationHours > 0 && request.VerificationType == herosms.VerificationCall:
		return HeroSMSCreateTasksInput{}, fmt.Errorf("%w: rental durations support sms verification only", storage.ErrInvalidInput)
	}
	country, err := strconv.Atoi(request.Country)
	if err != nil || country < 0 || country > 999 {
		return HeroSMSCreateTasksInput{}, fmt.Errorf("%w: country must be an integer between 0 and 999", storage.ErrInvalidInput)
	}
	productKind := domain.HeroSMSProductActivation
	if request.DurationHours != nil && *request.DurationHours > 0 {
		productKind = domain.HeroSMSProductRent
	}
	durationHours := cloneInt(request.DurationHours)
	if durationHours != nil && *durationHours == 0 {
		durationHours = nil
	}
	return HeroSMSCreateTasksInput{
		SubmissionID: submissionID, ProductKind: productKind, ServiceCode: request.Service,
		CountryCode: strconv.Itoa(country), VerificationType: string(request.VerificationType),
		DurationHours: durationHours, Quantity: request.Quantity,
	}, nil
}

func (h *HeroSMSAPI) enrichCreateInput(ctx context.Context, input HeroSMSCreateTasksInput) (HeroSMSCreateTasksInput, error) {
	client, err := h.newClient(ctx)
	if err != nil {
		// Price metadata is an optimization, not part of the idempotency
		// fingerprint. Let the durable task manager accept/replay the submission
		// even when the catalogue is temporarily unavailable.
		return input, nil
	}
	country, err := strconv.Atoi(input.CountryCode)
	if err != nil {
		return input, fmt.Errorf("%w: invalid country", storage.ErrInvalidInput)
	}
	duration := 0
	if input.DurationHours != nil {
		duration = *input.DurationHours
	}
	offers, err := client.Offers(ctx, herosms.OfferRequest{
		Service: input.ServiceCode, Country: country, DurationHours: duration,
		VerificationType: herosms.VerificationType(input.VerificationType),
	})
	if err != nil {
		return input, nil
	}
	if len(offers) == 0 {
		return input, nil
	}
	selected := offers[0]
	for _, offer := range offers[1:] {
		if selected.Count <= 0 && offer.Count > 0 {
			selected = offer
			break
		}
	}
	input.MaxPriceAmount = strings.TrimSpace(selected.Price)
	input.Currency = strings.TrimSpace(selected.Currency)
	return input, nil
}

func parseHeroSMSIdempotencyKey(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%w: Idempotency-Key is required", storage.ErrInvalidInput)
	}
	if len(value) > heroSMSMaxIdempotencyKey {
		return "", fmt.Errorf("%w: Idempotency-Key must not exceed %d characters", storage.ErrInvalidInput, heroSMSMaxIdempotencyKey)
	}
	for index, character := range value {
		allowed := character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.ContainsRune("-._~:/+", character)
		if !allowed || index == 0 && !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9') {
			return "", fmt.Errorf("%w: Idempotency-Key contains unsupported characters", storage.ErrInvalidInput)
		}
	}
	return value, nil
}

func (h *HeroSMSAPI) listTasks(c *gin.Context) {
	if h == nil || h.tasks == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "HeroSMS task manager is unavailable"})
		return
	}
	page, err := heroSMSTaskPage(c)
	if err != nil {
		respondError(c, err)
		return
	}
	tasks, err := h.tasks.ListTasks(c.Request.Context(), page)
	if err != nil {
		respondError(c, err)
		return
	}
	response := h.tasksResponse(tasks)
	if len(tasks) == page.Limit {
		response.NextCursor = strconv.Itoa(page.Offset + len(tasks))
	}
	c.JSON(http.StatusOK, response)
}

func heroSMSTaskPage(c *gin.Context) (storage.Page, error) {
	page := storage.Page{Limit: queryLimit(c, 500)}
	cursor := strings.TrimSpace(c.Query("cursor"))
	if cursor == "" {
		cursor = strings.TrimSpace(c.Query("offset"))
	}
	if cursor == "" {
		return page, nil
	}
	offset, err := strconv.Atoi(cursor)
	if err != nil || offset < 0 {
		return storage.Page{}, fmt.Errorf("%w: cursor must be a non-negative integer", storage.ErrInvalidInput)
	}
	page.Offset = offset
	return page, nil
}

func (h *HeroSMSAPI) startTask(c *gin.Context) {
	h.taskAction(c, func(ctx context.Context, id int64) (domain.HeroSMSNumberTask, error) {
		return h.tasks.StartTask(ctx, id)
	})
}

func (h *HeroSMSAPI) stopTask(c *gin.Context) {
	h.taskAction(c, func(ctx context.Context, id int64) (domain.HeroSMSNumberTask, error) {
		return h.tasks.StopTask(ctx, id)
	})
}

// Cancel and settle deliberately share the manager's stop decision. The
// manager rechecks messages and the refund deadline atomically before choosing
// provider cancellation or successful settlement, avoiding a stale browser
// forcing the wrong irreversible action.
func (h *HeroSMSAPI) cancelTask(c *gin.Context) {
	h.taskAction(c, func(ctx context.Context, id int64) (domain.HeroSMSNumberTask, error) {
		return h.tasks.StopTask(ctx, id)
	})
}

func (h *HeroSMSAPI) settleTask(c *gin.Context) {
	h.taskAction(c, func(ctx context.Context, id int64) (domain.HeroSMSNumberTask, error) {
		return h.tasks.StopTask(ctx, id)
	})
}

func (h *HeroSMSAPI) taskAction(c *gin.Context, action func(context.Context, int64) (domain.HeroSMSNumberTask, error)) {
	if h == nil || h.tasks == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "HeroSMS task manager is unavailable"})
		return
	}
	id, ok := pathID(c)
	if !ok {
		return
	}
	task, err := action(c.Request.Context(), id)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, h.tasksResponse([]domain.HeroSMSNumberTask{task}))
}

func (h *HeroSMSAPI) tasksResponse(tasks []domain.HeroSMSNumberTask) heroSMSTasksResponse {
	now := time.Now().UTC()
	if h != nil && h.now != nil {
		now = h.now().UTC()
	}
	views := make([]heroSMSTaskView, 0, len(tasks))
	for _, task := range tasks {
		views = append(views, newHeroSMSTaskView(task, now))
	}
	return heroSMSTasksResponse{Tasks: views, ServerNow: now}
}

func newHeroSMSTaskView(task domain.HeroSMSNumberTask, now time.Time) heroSMSTaskView {
	if task.Messages == nil {
		task.Messages = []domain.HeroSMSNumberMessage{}
	}
	messageCount := task.MessageCount
	if len(task.Messages) > messageCount {
		messageCount = len(task.Messages)
	}
	expiredByClock := task.ExpiresAt != nil && !now.Before(*task.ExpiresAt)
	refundable := task.ProviderActivationID != "" && task.RefundStatus == domain.HeroSMSRefundRefundable &&
		task.RefundableUntil != nil && now.Before(*task.RefundableUntil) && messageCount == 0 &&
		!task.Status.Terminal() && !expiredByClock
	running := !task.Status.Terminal() && !expiredByClock
	canStart := task.Status == domain.HeroSMSTaskStopped && task.ProviderActivationID == "" && task.PurchaseToken == ""
	actionable := !task.StopRequested && task.Status != domain.HeroSMSTaskSettling
	canStop := running && actionable && task.ProviderActivationID == ""
	canCancel := running && actionable && task.ProviderActivationID != "" && refundable
	canSettle := running && actionable && task.ProviderActivationID != "" && !refundable
	view := heroSMSTaskView{
		HeroSMSNumberTask: task,
		Refundable:        refundable,
		Running:           running,
		Capabilities: heroSMSTaskCapabilities{
			Start: canStart, Stop: canStop, Cancel: canCancel, Settle: canSettle,
		},
	}
	if task.DurationHours != nil {
		seconds := int64(*task.DurationHours) * int64(time.Hour/time.Second)
		view.RequestedDurationSeconds = &seconds
	}
	if task.PurchasedAt != nil && task.ExpiresAt != nil && task.ExpiresAt.After(*task.PurchasedAt) {
		seconds := int64(task.ExpiresAt.Sub(*task.PurchasedAt) / time.Second)
		view.EffectiveDurationSeconds = &seconds
	}
	return view
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
