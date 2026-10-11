// Package recoveryhttp supplies an unwired operator restore endpoint. The host
// must mount it on its authenticated operator surface, never a consumer route.
package recoveryhttp

import (
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/spencercnorton/bitagent/internal/cataloguerecovery"
	"net/http"
	"strconv"
	"time"
)

type Handler struct {
	store        *cataloguerecovery.Store
	authenticate gin.HandlerFunc
}

// New requires the host's explicit operator authentication middleware.
func New(store *cataloguerecovery.Store, authenticate gin.HandlerFunc) *Handler {
	return &Handler{store: store, authenticate: authenticate}
}
func (h *Handler) Apply(e *gin.Engine) error {
	if h.store == nil || !h.store.Enabled() {
		return cataloguerecovery.ErrDisabled
	}
	if h.authenticate == nil {
		return fmt.Errorf("catalogue recovery: operator authentication required")
	}
	e.GET("/catalogue-recovery/classifier-deletes", h.authenticate, func(c *gin.Context) {
		limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
		offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
		if limit <= 0 || limit > 100 {
			limit = 50
		}
		if offset < 0 {
			offset = 0
		}
		deletes, total, err := h.store.ListClassifierDeletes(c.Request.Context(), limit, offset)
		if err != nil {
			if errors.Is(err, cataloguerecovery.ErrDisabled) {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "catalogue recovery disabled"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list classifier deletes"})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"deletes": deletes,
			"total":   total,
			"limit":   limit,
			"offset":  offset,
		})
	})
	e.POST("/catalogue-recovery/:id/restore", h.authenticate, func(c *gin.Context) {
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil || id <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid snapshot ID"})
			return
		}
		restored, err := h.store.Restore(c.Request.Context(), id, time.Now().UTC())
		if err != nil {
			status := http.StatusInternalServerError
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				status = http.StatusNotFound
			case errors.Is(err, cataloguerecovery.ErrExpired):
				status = http.StatusGone
			case errors.Is(err, cataloguerecovery.ErrConflict) || errors.Is(err, cataloguerecovery.ErrProtected):
				status = http.StatusConflict
			case errors.Is(err, cataloguerecovery.ErrCapacity):
				status = http.StatusInsufficientStorage
			}
			c.JSON(status, gin.H{"error": "restore failed; snapshot retained"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"restored": restored, "snapshot_id": id})
	})
	return nil
}
