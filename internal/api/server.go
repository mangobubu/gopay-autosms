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
	proxyaddr "github.com/mangobubu/gopay-autosms/internal/proxy"
	appsettings "github.com/mangobubu/gopay-autosms/internal/settings"
	"github.com/mangobubu/gopay-autosms/internal/smsbower"
	"github.com/mangobubu/gopay-autosms/internal/storage"
	"github.com/mangobubu/gopay-autosms/internal/workflow"
)

type Server struct {
	store    storage.Store
	settings *appsettings.Manager
	workflow *workflow.Manager
}

func NewRouter(store storage.Store, settings *appsettings.Manager, manager *workflow.Manager, spa http.Handler) *gin.Engine {
	server := &Server{store: store, settings: settings, workflow: manager}
	router := gin.New()
	router.Use(gin.Logger(), gin.Recovery())
	router.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	router.GET("/readyz", func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		if err := store.Ping(ctx); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready", "error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	})

	server.register(router.Group("/api"))
	server.register(router.Group("/api/v1"))
	if spa != nil {
		router.NoRoute(gin.WrapH(spa))
	}
	return router
}

func (s *Server) register(group *gin.RouterGroup) {
	group.GET("/settings/smsbower", s.getSMSBowerSettings)
	group.PUT("/settings/smsbower", s.putSMSBowerSettings)
	group.POST("/settings/smsbower/test", s.testSMSBowerSettings)
	group.GET("/catalog/services", s.listServices)
	group.GET("/catalog/countries", s.listCountries)
	group.GET("/catalog/prices", s.listPrices)

	group.POST("/batches", s.createBatch)
	group.POST("/jobs", s.createBatch)
	group.GET("/batches", s.listBatches)
	group.GET("/jobs", s.listBatches)
	group.GET("/batches/:id", s.getBatch)
	group.GET("/jobs/:id", s.getBatch)
	group.POST("/batches/:id/stop", s.stopBatch)
	group.POST("/jobs/:id/stop", s.stopBatch)

	group.GET("/activations", s.listActivations)
	group.GET("/activations/:id", s.getActivation)
	group.POST("/activations/:id/success", s.markSuccess)
	group.DELETE("/activations/:id", s.deleteActivation)
	group.GET("/accounts", s.listAccounts)
	group.GET("/dashboard", s.dashboard)
}

func (s *Server) getSMSBowerSettings(c *gin.Context) {
	value, err := s.settings.GetSMSBower(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"api_key":    appsettings.MaskAPIKey(value.APIKey),
		"configured": strings.TrimSpace(value.APIKey) != "",
	})
}

func (s *Server) putSMSBowerSettings(c *gin.Context) {
	var request struct {
		APIKey string `json:"api_key"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		respondError(c, fmt.Errorf("invalid settings: %w", err))
		return
	}
	value, err := s.settings.SetSMSBower(c.Request.Context(), appsettings.SMSBower{APIKey: request.APIKey})
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"api_key": appsettings.MaskAPIKey(value.APIKey), "configured": value.APIKey != ""})
}

func (s *Server) testSMSBowerSettings(c *gin.Context) {
	client, err := s.newSMSClient(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	services, err := client.GetServicesList(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "services": len(services)})
}

func (s *Server) listServices(c *gin.Context) {
	client, err := s.newSMSClient(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	services, err := client.GetServicesList(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"services": services})
}

func (s *Server) listCountries(c *gin.Context) {
	client, err := s.newSMSClient(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	countries, err := client.GetCountries(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"countries": countries})
}

func (s *Server) listPrices(c *gin.Context) {
	country, err := strconv.Atoi(c.Query("country"))
	if err != nil {
		respondError(c, fmt.Errorf("country must be a numeric SMSBower country ID"))
		return
	}
	client, err := s.newSMSClient(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	prices, err := client.GetPrices(c.Request.Context(), smsbower.PriceRequest{Service: c.Query("service"), Country: country})
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"prices": prices})
}

type createBatchRequest struct {
	Service      string  `json:"service" binding:"required"`
	ServiceName  string  `json:"service_name"`
	Country      string  `json:"country" binding:"required"`
	CountryName  string  `json:"country_name"`
	Price        float64 `json:"price"`
	MaxPrice     string  `json:"max_price"`
	Provider     string  `json:"provider"`
	ProviderIDs  []int64 `json:"provider_ids"`
	Quantity     int     `json:"quantity" binding:"required,min=1"`
	PIN          string  `json:"pin" binding:"required"`
	Proxy        string  `json:"proxy"`
	ProxyPool    string  `json:"proxy_pool"`
	Currency     string  `json:"currency"`
	GoPayCountry string  `json:"gopay_country_code"`
}

func (s *Server) createBatch(c *gin.Context) {
	var request createBatchRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		respondError(c, fmt.Errorf("invalid batch: %w", err))
		return
	}
	if err := domain.ValidatePIN(request.PIN); err != nil {
		respondError(c, err)
		return
	}
	if request.MaxPrice == "" && request.Price > 0 {
		request.MaxPrice = strconv.FormatFloat(request.Price, 'f', -1, 64)
	}
	if request.MaxPrice == "" {
		respondError(c, fmt.Errorf("price is required"))
		return
	}
	if len(request.ProviderIDs) == 0 && request.Provider != "" {
		if providerID, err := strconv.ParseInt(request.Provider, 10, 64); err == nil {
			request.ProviderIDs = []int64{providerID}
		}
	}
	proxyText := request.ProxyPool
	if proxyText == "" {
		proxyText = request.Proxy
	}
	proxyEntries, err := proxyaddr.ParseLines(proxyText)
	if err != nil {
		respondError(c, err)
		return
	}
	batch, err := s.workflow.CreateBatch(c.Request.Context(), workflow.CreateBatchInput{
		ServiceCode: request.Service, ServiceName: request.ServiceName,
		CountryCode: request.Country, CountryName: request.CountryName,
		MaxPrice: request.MaxPrice, Currency: request.Currency, Quantity: request.Quantity, PIN: request.PIN,
		Options: workflow.BatchOptions{ProviderIDs: request.ProviderIDs, MinPrice: request.MaxPrice, GoPayCountryCode: request.GoPayCountry},
		Proxies: proxyEntries,
	})
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusCreated, batch)
}

func (s *Server) listBatches(c *gin.Context) {
	batches, err := s.store.ListBatches(c.Request.Context(), storage.BatchFilter{Page: storage.Page{Limit: queryLimit(c, 100)}})
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"batches": batches})
}

func (s *Server) getBatch(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	batch, err := s.store.GetBatch(c.Request.Context(), id)
	if err != nil {
		respondError(c, err)
		return
	}
	activations, err := s.activationViews(c.Request.Context(), storage.ActivationFilter{BatchID: &id, Page: storage.Page{Limit: 500}})
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"batch": batch, "activations": activations})
}

func (s *Server) stopBatch(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	updated, err := s.store.CancelBatch(c.Request.Context(), id)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, updated)
}

func (s *Server) listActivations(c *gin.Context) {
	filter := storage.ActivationFilter{Page: storage.Page{Limit: queryLimit(c, 200)}, IncludeHidden: c.Query("include_hidden") == "true"}
	if raw := c.Query("batch_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			respondError(c, fmt.Errorf("invalid batch_id"))
			return
		}
		filter.BatchID = &id
	}
	views, err := s.activationViews(c.Request.Context(), filter)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"activations": views})
}

func (s *Server) getActivation(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	activation, err := s.store.GetActivation(c.Request.Context(), id)
	if err != nil {
		respondError(c, err)
		return
	}
	view, err := s.activationView(c.Request.Context(), activation)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, view)
}

func (s *Server) markSuccess(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	if err := s.workflow.MarkSuccess(c.Request.Context(), id); err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"status": "accepted"})
}

func (s *Server) deleteActivation(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	if err := s.workflow.Delete(c.Request.Context(), id); err != nil {
		respondError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (s *Server) listAccounts(c *gin.Context) {
	accounts, err := s.store.ListAccounts(c.Request.Context(), storage.AccountFilter{Page: storage.Page{Limit: queryLimit(c, 200)}})
	if err != nil {
		respondError(c, err)
		return
	}
	public := make([]gin.H, 0, len(accounts))
	for _, account := range accounts {
		public = append(public, gin.H{
			"id": account.ID, "phone_number": account.PhoneNumber,
			"status": account.Status, "balance_rp": account.BalanceRP,
			"last_login_at": account.LastLoginAt, "created_at": account.CreatedAt,
			"updated_at": account.UpdatedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"accounts": public})
}

func (s *Server) dashboard(c *gin.Context) {
	batches, err := s.store.ListBatches(c.Request.Context(), storage.BatchFilter{Page: storage.Page{Limit: 20}})
	if err != nil {
		respondError(c, err)
		return
	}
	views, err := s.activationViews(c.Request.Context(), storage.ActivationFilter{Page: storage.Page{Limit: 200}})
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"batches": batches, "activations": views})
}

func (s *Server) newSMSClient(ctx context.Context) (*smsbower.Client, error) {
	value, err := s.settings.GetSMSBower(ctx)
	if err != nil {
		return nil, err
	}
	return smsbower.NewClient(smsbower.Config{APIKey: value.APIKey, BaseURL: value.BaseURL})
}

func (s *Server) activationViews(ctx context.Context, filter storage.ActivationFilter) ([]gin.H, error) {
	items, err := s.store.ListActivations(ctx, filter)
	if err != nil {
		return nil, err
	}
	views := make([]gin.H, 0, len(items))
	for _, item := range items {
		view, err := s.activationView(ctx, item)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

func (s *Server) activationView(ctx context.Context, activation domain.Activation) (gin.H, error) {
	codes, err := s.store.ListVerificationCodes(ctx, activation.ID)
	if err != nil {
		return nil, err
	}
	var loginCode, pinCode string
	subsequent := make([]gin.H, 0)
	for _, code := range codes {
		switch code.Phase {
		case domain.VerificationPhaseLogin:
			loginCode = code.Code
		case domain.VerificationPhasePIN:
			pinCode = code.Code
		case domain.VerificationPhaseSubsequent:
			subsequent = append(subsequent, gin.H{"id": code.ID, "ordinal": code.Ordinal, "code": code.Code, "received_at": code.CreatedAt})
		}
	}
	publicCodes := make([]gin.H, 0, len(codes))
	for _, code := range codes {
		publicCodes = append(publicCodes, gin.H{
			"id": code.ID, "phase": code.Phase, "ordinal": code.Ordinal,
			"code": code.Code, "created_at": code.CreatedAt,
		})
	}
	return gin.H{
		"id": activation.ID, "activation_id": activation.ProviderActivationID,
		"batch_id": activation.BatchID, "phone_number": activation.PhoneNumber,
		"service_code": activation.ServiceCode, "country_code": activation.CountryCode,
		"provider": activation.Provider, "operator": activation.Operator,
		"purchase_price": activation.PurchasePriceAmount, "currency": activation.Currency,
		"status": activation.Status, "balance_rp": activation.BalanceRP,
		"login_code": loginCode, "pin_code": pinCode, "subsequent_codes": subsequent,
		"verification_codes": publicCodes, "error": activation.FailureReason,
		"created_at": activation.CreatedAt, "expires_at": activation.ProviderExpiresAt,
		"finished_at":    activation.FinishedAt,
		"ever_fulfilled": activation.EverFulfilled,
	}, nil
}

func pathID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		respondError(c, fmt.Errorf("invalid id"))
		return 0, false
	}
	return id, true
}

func queryLimit(c *gin.Context, fallback int) int {
	value, err := strconv.Atoi(c.DefaultQuery("limit", strconv.Itoa(fallback)))
	if err != nil || value <= 0 {
		return fallback
	}
	if value > 500 {
		return 500
	}
	return value
}

func respondError(c *gin.Context, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, storage.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, storage.ErrConflict), errors.Is(err, storage.ErrBatchCapacity):
		status = http.StatusConflict
	case errors.Is(err, storage.ErrInvalidInput), errors.Is(err, domain.ErrInvalidPIN), errors.Is(err, domain.ErrInvalidPhone):
		status = http.StatusBadRequest
	default:
		message := strings.ToLower(err.Error())
		if strings.Contains(message, "invalid") || strings.Contains(message, "required") || strings.Contains(message, "未配置") {
			status = http.StatusBadRequest
		}
	}
	c.JSON(status, gin.H{"error": err.Error()})
}
