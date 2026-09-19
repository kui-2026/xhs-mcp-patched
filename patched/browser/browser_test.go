package browser

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOptions(t *testing.T) {
	cfg := &browserConfig{}
	WithFingerprintSeed(98759)(cfg)
	WithProxy("http://127.0.0.1:8080")(cfg)
	WithUserDataDir(" C:\\xhs-mcp\\chrome-profile ")(cfg)
	assert.Equal(t, 98759, cfg.fingerprintSeed)
	assert.Equal(t, "http://127.0.0.1:8080", cfg.proxy)
	assert.Equal(t, "C:\\xhs-mcp\\chrome-profile", cfg.userDataDir)
}

func TestMaskProxyCredentials(t *testing.T) {
	assert.Equal(t, "http://***:***@host:8080", maskProxyCredentials("http://user:pass@host:8080"))
}
