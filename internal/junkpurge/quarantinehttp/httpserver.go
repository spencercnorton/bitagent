// Package quarantinehttp exposes the junkpurge quarantine review API on the
// shared Gin server: list pending entries, restore one (undo), or delete one
// now. Consumed by the bitagent-ui "Quarantine" tab.
package quarantinehttp

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/httpserver"
	"github.com/spencercnorton/bitagent/internal/junkpurge"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/verdicts"
	"go.uber.org/zap"
)

// New builds the Gin option. Provided into the http_server_options group.
func New(pool lazy.Lazy[*pgxpool.Pool], daoQ lazy.Lazy[*dao.Query], cfg junkpurge.Config, vstore *verdicts.Store, logger *zap.SugaredLogger) httpserver.Option {
	return builder{pool: pool, dao: daoQ, cfg: cfg, verdicts: vstore, logger: logger.Named("quarantinehttp")}
}

type builder struct {
	pool     lazy.Lazy[*pgxpool.Pool]
	dao      lazy.Lazy[*dao.Query]
	cfg      junkpurge.Config
	verdicts *verdicts.Store
	logger   *zap.SugaredLogger
}

func (builder) Key() string { return "junkpurge-quarantine" }

func (b builder) Apply(e *gin.Engine) error {
	h := &handler{pool: b.pool, dao: b.dao, cfg: b.cfg, verdicts: b.verdicts, logger: b.logger}
	g := e.Group("/api/quarantine")
	g.GET("", h.list)
	g.POST("/:hash/restore", h.restore)
	g.DELETE("/:hash", h.deleteNow)
	return nil
}

type handler struct {
	pool     lazy.Lazy[*pgxpool.Pool]
	dao      lazy.Lazy[*dao.Query]
	cfg      junkpurge.Config
	verdicts *verdicts.Store
	logger   *zap.SugaredLogger
}

func (h *handler) quarantineDays() int {
	if h.cfg.QuarantineDays > 0 {
		return h.cfg.QuarantineDays
	}
	return 30
}

func (h *handler) list(c *gin.Context) {
	pool, err := h.pool.Get()
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	items, total, err := junkpurge.ListQuarantine(c.Request.Context(), pool, h.quarantineDays(), limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "totalCount": total, "windowDays": h.quarantineDays()})
}

func (h *handler) restore(c *gin.Context) {
	pool, err := h.pool.Get()
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	daoQ, err := h.dao.Get()
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	if err := junkpurge.RestoreQuarantined(c.Request.Context(), pool, daoQ, h.verdicts, h.logger, c.Param("hash")); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "restored", "infoHash": c.Param("hash")})
}

func (h *handler) deleteNow(c *gin.Context) {
	pool, err := h.pool.Get()
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	if err := junkpurge.DeleteQuarantinedNow(c.Request.Context(), pool, h.verdicts, h.logger, c.Param("hash")); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "deleted", "infoHash": c.Param("hash")})
}
