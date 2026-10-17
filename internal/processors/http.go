package processors

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	v1 "github.com/spencercnorton/bitagent/processorapi/v1"
)

func (s *Store) Key() string { return "processors" }
func (s *Store) Apply(e *gin.Engine) error {
	g := e.Group("/api/processors/v1", func(c *gin.Context) {
		if s.cfg.AdminToken == "" {
			c.AbortWithStatusJSON(503, gin.H{"error": "administration_disabled"})
			return
		}
		expected := "Bearer " + string(s.cfg.AdminToken)
		if subtle.ConstantTimeCompare([]byte(c.GetHeader("Authorization")), []byte(expected)) != 1 {
			c.AbortWithStatusJSON(401, gin.H{"error": "unauthorized"})
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), s.cfg.RequestTimeout+3*time.Second)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	g.GET("/status", func(c *gin.Context) {
		status, err := s.Status(c.Request.Context())
		if err != nil {
			apiError(c, err)
			return
		}
		c.JSON(200, status)
	})
	g.GET("/capabilities", func(c *gin.Context) {
		c.JSON(200, v1.OperationsCapabilities{Contract: v1.OperationsContract, Features: map[string]string{
			"terminal_attempt_window": "latest-500-v1", "admission": v1.AdmissionContract,
			"terminal_attempt_prefix": "complete-startup-prefix-v1",
			"job_export":              v1.JobExportContract,
			"reprocessing":            v1.ReprocessingContract,
			"preparation_recovery":    "bounded-local-retries-v1",
			"core_operations":         v1.CoreOpsContract,
			"feedback":                v1.FeedbackContract,
			"stacking":                "selector-priority-v1",
			"builtin_drivers":         "v1",
		}})
	})
	g.GET("/admission", func(c *gin.Context) {
		p, err := s.Admission(c.Request.Context())
		if err != nil {
			apiError(c, err)
			return
		}
		c.JSON(200, p)
	})
	g.PUT("/admission", func(c *gin.Context) {
		var w v1.AdmissionWrite
		if !body(c, &w) {
			return
		}
		p, err := s.SetAdmission(c.Request.Context(), w)
		if err != nil {
			apiError(c, err)
			return
		}
		c.JSON(200, p)
	})
	g.GET("/admission/history", func(c *gin.Context) {
		before, err := strconv.ParseInt(c.DefaultQuery("before", "0"), 10, 64)
		limit, limitErr := strconv.Atoi(c.DefaultQuery("limit", "50"))
		if err != nil || limitErr != nil {
			apiError(c, ErrControl)
			return
		}
		history, err := s.AdmissionHistory(c.Request.Context(), before, limit)
		if err != nil {
			apiError(c, err)
			return
		}
		c.JSON(200, history)
	})
	g.GET("/registry", func(c *gin.Context) {
		status, err := s.Status(c.Request.Context())
		if err != nil {
			apiError(c, err)
			return
		}
		c.JSON(200, status.Processors)
	})
	g.GET("/registry/:id", s.registryStatus)
	g.PUT("/registry/:id", func(c *gin.Context) {
		var w v1.RegistryWrite
		if !body(c, &w) {
			return
		}
		_, err := s.Register(c.Request.Context(), c.Param("id"), w)
		if err != nil {
			if errors.Is(err, ErrRegistration) {
				c.JSON(422, gin.H{"error": "registration_rejected"})
			} else {
				apiError(c, err)
			}
			return
		}
		s.registryStatus(c)
	})
	g.PATCH("/registry/:id", func(c *gin.Context) {
		var w v1.RegistryControl
		if !body(c, &w) {
			return
		}
		if w.ExpectedGeneration <= 0 || (w.State == nil && w.Mode == nil && w.Selector == nil && w.Learning == nil) {
			c.JSON(400, gin.H{"error": "invalid_control"})
			return
		}
		if err := s.Control(c.Request.Context(), c.Param("id"), w); err != nil {
			apiError(c, err)
			return
		}
		s.registryStatus(c)
	})
	g.DELETE("/registry/:id", func(c *gin.Context) {
		gen, err := strconv.ParseInt(c.Query("expected_generation"), 10, 64)
		if err != nil || gen <= 0 {
			c.JSON(400, gin.H{"error": "invalid_generation"})
			return
		}
		if err = s.Remove(c.Request.Context(), c.Param("id"), gen); err != nil {
			apiError(c, err)
			return
		}
		c.Status(204)
	})
	g.GET("/jobs", func(c *gin.Context) {
		limit, err := strconv.Atoi(c.DefaultQuery("limit", "50"))
		if err != nil || limit < 1 || limit > 100 {
			c.JSON(400, gin.H{"error": "invalid_limit"})
			return
		}
		page, err := s.Jobs(c.Request.Context(), limit, c.Query("cursor"), c.Query("processor_id"))
		if err != nil {
			apiError(c, err)
			return
		}
		c.JSON(200, page)
	})
	g.POST("/jobs/:job_id/cancel", func(c *gin.Context) {
		state, err := s.Cancel(c.Request.Context(), c.Param("job_id"))
		if err != nil {
			apiError(c, err)
			return
		}
		c.JSON(200, gin.H{"job_id": c.Param("job_id"), "state": state})
	})
	g.GET("/jobs/:job_id/export", func(c *gin.Context) {
		export, err := s.ExportJob(c.Request.Context(), c.Param("job_id"))
		if err != nil {
			apiError(c, err)
			return
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(200, export)
	})
	g.POST("/jobs/:job_id/reprocess", func(c *gin.Context) {
		var w v1.ReprocessWrite
		if !body(c, &w) {
			return
		}
		status, err := s.Reprocess(c.Request.Context(), c.Param("job_id"), w)
		if err != nil {
			apiError(c, err)
			return
		}
		c.JSON(200, status)
	})
	g.GET("/reprocessing/:request_id", func(c *gin.Context) {
		status, err := s.Reprocessing(c.Request.Context(), c.Param("request_id"))
		if err != nil {
			apiError(c, err)
			return
		}
		c.JSON(200, status)
	})
	g.GET("/reprocessing/:request_id/export", func(c *gin.Context) {
		audit, err := s.ExportReprocessing(c.Request.Context(), c.Param("request_id"))
		if err != nil {
			apiError(c, err)
			return
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(200, audit)
	})
	g.POST("/reprocessing/:request_id/cancel", func(c *gin.Context) {
		status, err := s.CancelReprocessing(c.Request.Context(), c.Param("request_id"))
		if err != nil {
			apiError(c, err)
			return
		}
		c.JSON(200, status)
	})
	g.POST("/core/parse", func(c *gin.Context) {
		var req v1.CoreParseRequest
		if !body(c, &req) {
			return
		}
		out, err := s.CoreParse(c.Request.Context(), req)
		if err != nil {
			apiError(c, err)
			return
		}
		c.JSON(200, out)
	})
	g.POST("/core/search", func(c *gin.Context) {
		var req v1.CoreSearchRequest
		if !body(c, &req) {
			return
		}
		out, err := s.CoreSearch(c.Request.Context(), req)
		if err != nil {
			apiError(c, err)
			return
		}
		c.JSON(200, out)
	})
	g.POST("/core/liveness", func(c *gin.Context) {
		var req v1.CoreLivenessRequest
		if !body(c, &req) {
			return
		}
		out, err := s.CoreLiveness(c.Request.Context(), req)
		if err != nil {
			apiError(c, err)
			return
		}
		c.JSON(200, out)
	})
	g.POST("/core/policy", func(c *gin.Context) {
		var req v1.CorePolicyRequest
		if !body(c, &req) {
			return
		}
		out, err := s.CorePolicy(c.Request.Context(), req)
		if err != nil {
			apiError(c, err)
			return
		}
		c.JSON(200, out)
	})
	g.GET("/feedback", func(c *gin.Context) {
		after, err := strconv.ParseInt(c.DefaultQuery("after_seq", "0"), 10, 64)
		limit, limitErr := strconv.Atoi(c.DefaultQuery("limit", "50"))
		if err != nil || limitErr != nil {
			apiError(c, ErrControl)
			return
		}
		page, err := s.Feedback(c.Request.Context(), c.Query("processor_id"), after, limit, c.Query("tier"), c.Query("consumer_id"))
		if err != nil {
			apiError(c, err)
			return
		}
		c.JSON(200, page)
	})
	g.POST("/feedback", func(c *gin.Context) {
		var w v1.FeedbackWrite
		if !body(c, &w) {
			return
		}
		events, err := s.RecordFeedback(c.Request.Context(), w)
		if err != nil {
			apiError(c, err)
			return
		}
		c.JSON(200, gin.H{"contract": v1.FeedbackContract, "events": events})
	})
	g.POST("/feedback/ack", func(c *gin.Context) {
		var ack v1.FeedbackCursorAck
		if !body(c, &ack) {
			return
		}
		if err := s.AckFeedback(c.Request.Context(), ack); err != nil {
			apiError(c, err)
			return
		}
		c.JSON(200, gin.H{"contract": v1.FeedbackContract, "acknowledged": true})
	})
	return nil
}
func body(c *gin.Context, v any) bool {
	raw, e := io.ReadAll(io.LimitReader(c.Request.Body, 16385))
	if e != nil || len(raw) > 16384 || v1.Decode(raw, v) != nil {
		c.JSON(400, gin.H{"error": "invalid_json"})
		return false
	}
	return true
}
func apiError(c *gin.Context, e error) {
	code, reason := 503, "storage_unavailable"
	if errors.Is(e, ErrAbsent) || errors.Is(e, pgx.ErrNoRows) {
		code, reason = 404, "not_found"
	}
	if errors.Is(e, ErrConflict) || errors.Is(e, ErrStale) {
		code, reason = 409, "generation_conflict"
	}
	if errors.Is(e, ErrCapacity) {
		code, reason = 429, "capacity_exhausted"
	}
	if errors.Is(e, ErrControl) {
		code, reason = 400, "invalid_control"
	}
	if errors.Is(e, errCursor) {
		code, reason = 400, "invalid_cursor"
	}
	if errors.Is(e, errExportLimit) {
		code, reason = 413, "export_limit"
	}
	c.JSON(code, gin.H{"error": reason})
}
func (s *Store) registryStatus(c *gin.Context) {
	status, err := s.Status(c.Request.Context())
	if err != nil {
		apiError(c, err)
		return
	}
	for _, p := range status.Processors {
		if p.ID == c.Param("id") {
			c.JSON(http.StatusOK, p)
			return
		}
	}
	apiError(c, ErrAbsent)
}

var errCursor = errors.New("invalid_cursor")

func (s *Store) Jobs(ctx context.Context, limit int, cursor, processor string) (v1.JobPage, error) {
	out := v1.JobPage{Items: []v1.JobSummary{}}
	if len(cursor) > 256 || len(processor) > 96 {
		return out, errCursor
	}
	before := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	last := ""
	if cursor != "" {
		b, e := base64.RawURLEncoding.DecodeString(cursor)
		if e != nil {
			return out, errCursor
		}
		parts := strings.Split(string(b), "|")
		if len(parts) != 2 {
			return out, errCursor
		}
		before, e = time.Parse(time.RFC3339Nano, parts[0])
		if e != nil {
			return out, errCursor
		}
		last = parts[1]
	}
	p, e := s.pool.Get()
	if e != nil {
		return out, e
	}
	// Filtering is performed before LIMIT and may match any step in a pipeline.
	rows, e := p.Query(ctx, `SELECT job_id,input_revision,pipeline_revision,state,step,attempts,preparation_failures,created_at,available_at,lease_until,outcome,reason,plan FROM processor_jobs WHERE (created_at,job_id)<($1,$2) AND ($3='' OR EXISTS(SELECT 1 FROM jsonb_array_elements(plan->'steps') st WHERE st->'entry'->>'id'=$3)) ORDER BY created_at DESC,job_id DESC LIMIT $4`, before, last, processor, limit+1)
	if e != nil {
		return out, e
	}
	defer rows.Close()
	for rows.Next() {
		var j v1.JobSummary
		var step int
		var created, available time.Time
		var lease *time.Time
		var raw []byte
		if e = rows.Scan(&j.JobID, &j.InputRevision, &j.PipelineRevision, &j.State, &step, &j.Attempts, &j.PreparationFailures, &created, &available, &lease, &j.Outcome, &j.Reason, &raw); e != nil {
			return out, e
		}
		if len(out.Items) == limit {
			prior := out.Items[len(out.Items)-1]
			out.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(prior.CreatedAt + "|" + prior.JobID))
			break
		}
		var plan Plan
		if e = json.Unmarshal(raw, &plan); e != nil {
			return out, e
		}
		if len(plan.Steps) > 0 {
			st := plan.Steps[min(step, len(plan.Steps)-1)]
			j.Stage = st.Stage
			j.ProcessorID = st.Entry.ID
			j.Processor = st.Entry.Manifest.Processor
			j.ExecutionGeneration = st.Entry.ExecutionGeneration
		}
		j.CreatedAt = stamp(created)
		j.AvailableAt = stamp(available)
		if lease != nil {
			x := stamp(*lease)
			j.LeaseUntil = &x
		}
		out.Items = append(out.Items, j)
	}
	return out, rows.Err()
}
