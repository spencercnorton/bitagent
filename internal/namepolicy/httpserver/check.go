package httpserver

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/spencercnorton/bitagent/internal/httpserver"
	"github.com/spencercnorton/bitagent/internal/namepolicy"
	"github.com/spencercnorton/bitagent/internal/protocol"
)

const CheckPath = "/internal/name-policy/check"
const MaxBatch = 32
const MaxNameBytes = 4096
const MaxBodyBytes = 160 << 10

type Result struct {
	Index    int    `json:"index"`
	Eligible bool   `json:"eligible"`
	Reason   string `json:"reason"`
	Version  string `json:"version"`
}
type Response struct {
	Enabled bool     `json:"enabled"`
	Version string   `json:"version"`
	Results []Result `json:"results"`
}

type release struct {
	Name     string `json:"name"`
	InfoHash string `json:"infoHash,omitempty"`
}
type checkRequest struct {
	Releases []release `json:"releases"`
}

// New checks names owned by a trusted internal caller. It is not public hash
// authorization: it never substitutes a caller name for a stored public name.
func New(p *namepolicy.Policy) httpserver.Option { return builder{policy: p} }

type builder struct{ policy *namepolicy.Policy }

func (builder) Key() string                 { return "name-policy" }
func (b builder) Apply(e *gin.Engine) error { e.POST(CheckPath, CheckName(b.policy)); return nil }

// Authorize is shared with the separate stored-public-hash endpoint. It returns
// no configured secret and performs no database or provider operation.
func Authorize(c *gin.Context, p *namepolicy.Policy) bool {
	if !p.Enabled() || p.InternalToken() == "" {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "policy unavailable"})
		return false
	}
	value := c.GetHeader("Authorization")
	if !strings.HasPrefix(value, "Bearer ") || subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(value, "Bearer ")), []byte(string(p.InternalToken()))) != 1 {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		return false
	}
	return true
}

func Decode(c *gin.Context, out any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxBodyBytes)
	d := json.NewDecoder(c.Request.Body)
	d.DisallowUnknownFields()
	err := d.Decode(out)
	if err == nil {
		if end := d.Decode(new(any)); end != io.EOF {
			err = errors.New("trailing input")
		}
	}
	if err != nil {
		status := http.StatusBadRequest
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			status = http.StatusRequestEntityTooLarge
		}
		c.AbortWithStatusJSON(status, gin.H{"error": "invalid request"})
		return false
	}
	return true
}

func CheckName(p *namepolicy.Policy) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !Authorize(c, p) {
			return
		}
		var request checkRequest
		if !Decode(c, &request) {
			return
		}
		if len(request.Releases) < 1 || len(request.Releases) > MaxBatch {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
			return
		}
		results := make([]Result, 0, len(request.Releases))
		for index, item := range request.Releases {
			if !utf8.ValidString(item.Name) || strings.TrimSpace(item.Name) == "" || len(item.Name) > MaxNameBytes || strings.ContainsRune(item.Name, 0) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
				return
			}
			var hash protocol.ID
			if item.InfoHash != "" {
				var err error
				hash, err = protocol.ParseID(item.InfoHash)
				if err != nil {
					c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
					return
				}
			}
			decision := p.Evaluate(hash, item.Name)
			results = append(results, Result{index, decision.Eligible, decision.Reason, decision.Version})
		}
		c.JSON(http.StatusOK, Response{Enabled: true, Version: namepolicy.Version, Results: results})
	}
}
