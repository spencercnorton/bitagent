package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/spencercnorton/bitagent/internal/database/query"
	"github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/namepolicy"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/stretchr/testify/require"
)

type publicHashSearchStub struct {
	search.ServingSearch
	read func(context.Context, []protocol.ID, ...query.Option) (search.TorrentsWithMissingInfoHashesResult, error)
}

func (s publicHashSearchStub) TorrentsWithMissingInfoHashes(c context.Context, h []protocol.ID, opts ...query.Option) (search.TorrentsWithMissingInfoHashesResult, error) {
	return s.read(c, h, opts...)
}

func publicHashRequest(t *testing.T, p *namepolicy.Policy, s search.ServingSearch, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	e := gin.New()
	require.NoError(t, NewPublicHash(lazy.New(func() (search.ServingSearch, error) { return s, nil }), p).Apply(e))
	r := httptest.NewRequest(http.MethodPost, "/internal/name-policy/check-public", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	e.ServeHTTP(w, r)
	return w
}

func TestPublicHashChecksStoredNameAndKeepsUnknownAndPrivateIneligible(t *testing.T) {
	token := strings.Repeat("synthetic-internal-", 3)
	p, err := namepolicy.New(namepolicy.Config{Enabled: true, InternalCheckToken: namepolicy.SecretToken(token)})
	require.NoError(t, err)
	hashes := []protocol.ID{{1}, {2}, {3}, {4}}
	read := 0
	s := publicHashSearchStub{read: func(ctx context.Context, h []protocol.ID, _ ...query.Option) (search.TorrentsWithMissingInfoHashesResult, error) {
		read++
		_, hasDeadline := ctx.Deadline()
		require.True(t, hasDeadline)
		require.Equal(t, hashes, h)
		return search.TorrentsWithMissingInfoHashesResult{Torrents: []model.Torrent{
			{InfoHash: hashes[0], Name: "Synthetic.Ordinary.2026.mkv"},
			{InfoHash: hashes[2], Name: "Synthetic.测试.2026.mkv"},
			{InfoHash: hashes[3], Name: "Synthetic.Private.2026.mkv", Private: true},
		}}, nil
	}}
	values := []string{hashes[0].String(), hashes[1].String(), hashes[2].String(), hashes[3].String()}
	body, err := json.Marshal(map[string]any{"infoHashes": values})
	require.NoError(t, err)
	w := publicHashRequest(t, p, s, token, string(body))
	require.Equal(t, http.StatusOK, w.Code)
	var result Response
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
	require.True(t, result.Enabled)
	require.Equal(t, namepolicy.Version, result.Version)
	require.True(t, result.Results[0].Eligible, "raw/unclassified stored names can be authorized")
	for _, i := range []int{1, 2, 3} {
		require.False(t, result.Results[i].Eligible)
	}
	require.Equal(t, "not_served", result.Results[1].Reason, "unknown is not fabricated script/adult evidence")
	require.Equal(t, "not_served", result.Results[3].Reason)
	require.Equal(t, namepolicy.ReasonHan, result.Results[2].Reason, "a substitute allowed title cannot override actual stored name")
	require.Equal(t, 1, read)
	for _, value := range append(values, "Synthetic", token) {
		require.NotContains(t, w.Body.String(), value)
	}
}

func TestPublicHashDefaultOffAuthenticationAndInvalidRequestsDoNotRead(t *testing.T) {
	token := strings.Repeat("synthetic-internal-", 3)
	on, err := namepolicy.New(namepolicy.Config{Enabled: true, InternalCheckToken: namepolicy.SecretToken(token)})
	require.NoError(t, err)
	off, err := namepolicy.New(namepolicy.NewDefaultConfig())
	require.NoError(t, err)
	s := publicHashSearchStub{read: func(context.Context, []protocol.ID, ...query.Option) (search.TorrentsWithMissingInfoHashesResult, error) {
		t.Fatal("invalid request read database")
		return search.TorrentsWithMissingInfoHashesResult{}, nil
	}}
	h := protocol.ID{1}.String()
	valid := `{"infoHashes":["` + h + `"]}`
	require.Equal(t, http.StatusNotFound, publicHashRequest(t, off, s, token, valid).Code)
	require.Equal(t, http.StatusForbidden, publicHashRequest(t, on, s, "wrong", valid).Code)
	for _, body := range []string{`{"infoHashes":[]}`, `{"infoHashes":["bad"]}`, valid + `{}`, `{"infoHashes":["` + h + `"],"name":"Synthetic.Allowed"}`} {
		require.Equal(t, http.StatusBadRequest, publicHashRequest(t, on, s, token, body).Code)
	}
	tooMany, _ := json.Marshal(map[string]any{"infoHashes": make([]string, 33)})
	require.Equal(t, http.StatusBadRequest, publicHashRequest(t, on, s, token, string(tooMany)).Code)
	require.Equal(t, http.StatusRequestEntityTooLarge, publicHashRequest(t, on, s, token, `{"infoHashes":["`+strings.Repeat("f", publicHashBodyLimit)+`"]}`).Code)
}
