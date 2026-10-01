package model_test

import (
	"os"
	"testing"

	modeltests "github.com/QuantumNous/new-api/tests/model"
)

func TestManagedAudioUsesRealRedisMetadata(t *testing.T) {
	if os.Getenv("SOTC12_REAL_REDIS") != "1" {
		t.Skip("真实共享Redis专项由SOTC12_REAL_REDIS启用")
	}
	modeltests.VerifyManagedAudioCacheAndFailure(t)
}
