package httpserver

import (
	"github.com/gin-gonic/gin"
	"github.com/spencercnorton/bitagent/internal/httpserver"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/torznab"
)

func New(lazyClient lazy.Lazy[torznab.Client], config torznab.Config, metrics *Metrics) httpserver.Option {
	return builder{
		lazyClient: lazyClient,
		config:     config,
		metrics:    metrics,
	}
}

type builder struct {
	lazyClient lazy.Lazy[torznab.Client]
	config     torznab.Config
	metrics    *Metrics
}

func (builder) Key() string {
	return "torznab"
}

func (b builder) Apply(e *gin.Engine) error {
	client, err := b.lazyClient.Get()
	if err != nil {
		return err
	}

	h := handler{
		config:  b.config,
		client:  client,
		metrics: b.metrics,
	}
	e.GET("/torznab/*any", h.handleRequest)

	return nil
}
