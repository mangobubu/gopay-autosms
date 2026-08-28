package api

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	appsettings "github.com/mangobubu/gopay-autosms/internal/settings"
)

func (s *Server) getBatchDraft(c *gin.Context) {
	draft, err := s.settings.GetBatchDraft(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, draft)
}

func (s *Server) putBatchDraft(c *gin.Context) {
	var draft appsettings.BatchDraft
	if err := c.ShouldBindJSON(&draft); err != nil {
		respondError(c, fmt.Errorf("invalid batch draft: %w", err))
		return
	}
	updated, err := s.settings.SetBatchDraft(c.Request.Context(), draft)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, updated)
}
