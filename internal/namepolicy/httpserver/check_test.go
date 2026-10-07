package httpserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/spencercnorton/bitagent/internal/namepolicy"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/stretchr/testify/require"
)

func TestInternalPureNameCheckBoundedNoEchoAndPrivateHashAuthority(t *testing.T) {
	hash := protocol.ID{1}
	token := strings.Repeat("synthetic-key-", 3)
	p, err := namepolicy.New(namepolicy.Config{Enabled: true, ExcludedInfoHashes: []string{hash.String()}, InternalCheckToken: namepolicy.SecretToken(token)})
	require.NoError(t, err)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	require.NoError(t, New(p).Apply(router))
	body := fmt.Sprintf(`{"releases":[{"name":"Allowed.Café.2024.mkv"},{"name":"Synthetic.电影.ENG.mkv"},{"name":"Fetish.X.X.X.mkv"},{"name":"Allowed.Name.mkv","infoHash":%q}]}`, hash.String())
	request := httptest.NewRequest(http.MethodPost, CheckPath, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	out := httptest.NewRecorder()
	router.ServeHTTP(out, request)
	require.Equal(t, http.StatusOK, out.Code)
	var reply Response
	require.NoError(t, json.Unmarshal(out.Body.Bytes(), &reply))
	require.True(t, reply.Enabled)
	require.Equal(t, namepolicy.Version, reply.Version)
	require.Len(t, reply.Results, 4)
	for index, result := range reply.Results {
		require.Equal(t, index, result.Index)
		require.Equal(t, namepolicy.Version, result.Version)
	}
	require.True(t, reply.Results[0].Eligible)
	require.Equal(t, namepolicy.ReasonHan, reply.Results[1].Reason)
	require.Equal(t, namepolicy.ReasonAdultComposite, reply.Results[2].Reason)
	require.Equal(t, namepolicy.ReasonOwnerHash, reply.Results[3].Reason)
	for _, secret := range []string{"Café", "电影", "Fetish", hash.String(), token} {
		require.NotContains(t, out.Body.String(), secret)
	}
}

func TestInternalNameCheckRefusalsAreBoundedAndDoNotEcho(t *testing.T) {
	token := strings.Repeat("synthetic-key-", 3)
	for _, tc := range []struct {
		name, body, token   string
		enabled, configured bool
		status              int
	}{
		{"disabled", `{"releases":[{"name":"Private.Name"}]}`, token, false, true, http.StatusNotFound},
		{"unconfigured", `{"releases":[{"name":"Private.Name"}]}`, token, true, false, http.StatusNotFound},
		{"unauthorized", `{"releases":[{"name":"Private.Name"}]}`, "wrong", true, true, http.StatusForbidden},
		{"unknownfield", `{"releases":[{"name":"Private.Name","callerPolicy":true}]}`, token, true, true, http.StatusBadRequest},
		{"trailing", `{"releases":[{"name":"Private.Name"}]} {}`, token, true, true, http.StatusBadRequest},
		{"empty", `{"releases":[]}`, token, true, true, http.StatusBadRequest},
		{"missingname", `{"releases":[{"infoHash":"0000000000000000000000000000000000000000"}]}`, token, true, true, http.StatusBadRequest},
		{"badHash", `{"releases":[{"name":"Private.Name","infoHash":"bad"}]}`, token, true, true, http.StatusBadRequest},
		{"largeName", `{"releases":[{"name":"` + strings.Repeat("x", MaxNameBytes+1) + `"}]}`, token, true, true, http.StatusBadRequest},
		{"largeBody", `{"releases":[{"name":"` + strings.Repeat("x", MaxBodyBytes) + `"}]}`, token, true, true, http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := namepolicy.Config{Enabled: tc.enabled}
			if tc.configured {
				cfg.InternalCheckToken = namepolicy.SecretToken(token)
			}
			p, err := namepolicy.New(cfg)
			require.NoError(t, err)
			router := gin.New()
			require.NoError(t, New(p).Apply(router))
			request := httptest.NewRequest(http.MethodPost, CheckPath, strings.NewReader(tc.body))
			request.Header.Set("Authorization", "Bearer "+tc.token)
			out := httptest.NewRecorder()
			router.ServeHTTP(out, request)
			require.Equal(t, tc.status, out.Code)
			require.NotContains(t, out.Body.String(), "Private.Name")
			require.NotContains(t, out.Body.String(), token)
		})
	}
}
