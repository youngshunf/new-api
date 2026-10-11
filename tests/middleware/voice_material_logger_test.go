package middlewaretests_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVoiceMaterialLoggerRedactsCapabilityWithoutChangingRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var output bytes.Buffer
	previousWriter, previousStore := gin.DefaultWriter, service.VoiceMedia
	gin.DefaultWriter = &output
	t.Cleanup(func() { gin.DefaultWriter, service.VoiceMedia = previousWriter, previousStore })
	store, err := service.NewVoiceMediaStore(t.TempDir())
	require.NoError(t, err)
	service.VoiceMedia = store
	wav, err := os.ReadFile("../fixtures/voice-preview.wav")
	require.NoError(t, err)
	token, err := store.SaveMaterial(bytes.NewReader(wav))
	require.NoError(t, err)
	engine := gin.New()
	var receivedURI, receivedToken string
	engine.Use(func(c *gin.Context) {
		c.Next()
		receivedURI, receivedToken = c.Request.URL.RequestURI(), c.Param("token")
	})
	middleware.SetUpLogger(engine)
	engine.GET("/v1/audio/voice-materials/:token", controller.VoiceMaterial)
	server := httptest.NewServer(engine)
	t.Cleanup(server.Close)
	for _, tc := range []struct {
		path, secret, token string
		status              int
	}{
		{"/v1/audio/voice-materials/" + token + "?metadata=never-log-query", token, token, http.StatusOK},
		{"/v1/audio/voice-materials/invalid-capability?metadata=never-log-query", "invalid-capability", "invalid-capability", http.StatusNotFound},
		{"/v1/audio/voice-materials/unknown-capability/extra?metadata=never-log-query", "unknown-capability", "unknown-capability", http.StatusNotFound},
		{"/v1/audio/voice-materials?metadata=never-log-query", "never-log-query", "", http.StatusNotFound},
		{"/v1/audio/voice-materials/%69nvalid-encoded?metadata=never-log-query", "invalid-encoded", "invalid-encoded", http.StatusNotFound},
	} {
		output.Reset()
		response, err := server.Client().Get(server.URL + tc.path)
		require.NoError(t, err)
		payload, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		assert.Equal(t, tc.status, response.StatusCode)
		if tc.status == http.StatusOK {
			assert.Equal(t, wav, payload, "日志脱敏不能破坏真实单次取件")
		}
		assert.NotContains(t, output.String(), tc.secret)
		assert.NotContains(t, output.String(), "never-log-query")
		assert.Contains(t, output.String(), "/v1/audio/voice-materials/:token")
		// 仅格式化日志，编码后的业务URI和已解析的路由参数都不能改变。
		assert.Equal(t, tc.path, receivedURI)
		assert.Equal(t, tc.token, receivedToken)
	}
}
