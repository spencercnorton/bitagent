package processor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/stretchr/testify/require"
)

type languageReviewCapture struct {
	processorCaptureProbe
	decision    llmcapture.ContentFilterDecision
	decisionErr error
}

func mustQuoteReviewAnswer(t *testing.T, answer string) string {
	t.Helper()
	b, err := json.Marshal(answer)
	require.NoError(t, err)
	return string(b)
}

func (p *languageReviewCapture) RecordContentFilterDecision(_ context.Context, _ llmcapture.ResultReceipt, _ []byte, d llmcapture.ContentFilterDecision) error {
	p.decision = d
	return p.decisionErr
}

func TestProcessLanguageReviewPersistsTagWithContentWithoutDeletionOrBlocking(t *testing.T) {
	for _, tc := range []struct {
		name, live, title, answer string
		auditErr                  error
		captureErr                error
		wantTag                   bool
		wantCalls                 int32
	}{
		{"live review", "true", "Pelicula 2026", `{"is_english":false,"confidence":0.93,"reason":"spanish-article"}`, nil, nil, true, 1},
		{"shadow review", "false", "Pelicula 2026", `{"is_english":false,"confidence":0.93,"reason":"spanish-article"}`, nil, nil, false, 1},
		{"English track protection", "true", "Pelicula ENG 2026", `{"is_english":false,"confidence":0.93,"reason":"spanish-article"}`, nil, nil, false, 0},
		{"malformed response", "true", "Pelicula 2026", `{"is_english":false}`, nil, nil, false, 1},
		{"failed terminal audit", "true", "Pelicula 2026", `{"is_english":false,"confidence":0.93,"reason":"spanish-article"}`, errors.New("decision unavailable"), nil, false, 1},
		{"invalid capture", "true", "Pelicula 2026", `{"is_english":false,"confidence":0.93,"reason":"spanish-article"}`, nil, errors.New("capture unavailable"), false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hash := processTestHash(0x59)
			p, mock, blocker := newProcessTestProcessor(t, []model.Torrent{processTestTorrent(hash, tc.title, "mkv")}, processRunnerStub{run: func(model.Torrent) (classification.Result, error) { return classification.Result{}, nil }})
			calls := &atomic.Int32{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				// Quote the synthetic model JSON as message content.
				_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":` + mustQuoteReviewAnswer(t, tc.answer) + `}}]}`))
			}))
			defer server.Close()
			cfg := contentfilter.NewDefaultConfig()
			cfg.Enabled, cfg.LLMEnabled, cfg.LLMApiStyle, cfg.LLMBaseURL = true, true, "chat", server.URL
			cfg.LLMAction, cfg.LLMEnforce = contentfilter.LLMActionReview, tc.live
			capture := &languageReviewCapture{decisionErr: tc.auditErr}
			capture.err = tc.captureErr
			client := contentfilter.NewOpenAIClientWithPolicy("test", cfg.LLMModel, server.URL, "chat", cfg.LLMPromptVersion, "", cfg.LLMMaxOutputTokens, time.Second)
			p.contentFilter = contentfilter.NewWithLLMAdmission(cfg, client, contentfilter.LLMCallbacks{}, contentfilter.Admission{Budget: processorBudgetProbe{}, Capture: capture})
			p.privacy = &fakePrivacy{}
			if tc.auditErr == nil && tc.captureErr == nil {
				mock.ExpectBegin()
				mock.ExpectQuery(`SELECT .*english_audio_source.* FROM "torrent_contents"`).WillReturnRows(sqlmock.NewRows([]string{"id", "english_audio", "english_audio_source"}))
				mock.ExpectQuery(`INSERT INTO "torrent_contents"`).WillReturnRows(sqlmock.NewRows([]string{"published_at"}).AddRow(nil))
				if tc.wantTag {
					mock.ExpectExec(`INSERT INTO "torrent_tags"`).WithArgs(hash.Bytes(), contentfilter.LLMReviewTag, sqlmock.AnyArg(), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
				}
				mock.ExpectCommit()
			}
			err := p.Process(context.Background(), MessageParams{InfoHashes: []protocol.ID{hash}})
			if tc.auditErr != nil || tc.captureErr != nil {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.wantCalls, calls.Load())
			require.Empty(t, blocker.blockedHashes(), "review must never block the retained source")
			if tc.wantTag {
				require.True(t, capture.decision.WouldReview)
				require.False(t, capture.decision.WouldDrop)
				require.Equal(t, "review", capture.decision.LLMAction)
			}
			// The only expected writes are the ordinary content/tag transaction.
			// Any torrent DELETE or quarantine/purge write fails these expectations.
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
