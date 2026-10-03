package httpserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/99designs/gqlgen/graphql"
	"github.com/gin-gonic/gin"
	"github.com/spencercnorton/bitagent/internal/gql"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/stretchr/testify/require"
)

// The backend retains its real GraphQL API without serving a browser client.
func TestHeadlessGraphQLRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	b := builder{schema: lazy.New(func() (graphql.ExecutableSchema, error) {
		return gql.NewExecutableSchema(gql.Config{}), nil
	})}
	require.NoError(t, b.Apply(engine))

	post := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":"{ __typename }"}`))
	request.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(post, request)
	require.Equal(t, http.StatusOK, post.Code)
	require.JSONEq(t, `{"data":{"__typename":"Query"}}`, post.Body.String())

	get := httptest.NewRecorder()
	engine.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/graphql", nil))
	require.Equal(t, http.StatusNotFound, get.Code)
	require.NotContains(t, get.Header().Get("Content-Type"), "text/html")
}
