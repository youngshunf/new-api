package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLogPublishesAuthoritativeCreditAmount(t *testing.T) {
	logEntry := &Log{Quota: 500000}

	require.NoError(t, logEntry.AfterFind(nil))
	assert.Equal(t, "1", logEntry.Credits)

	payload, err := common.Marshal(logEntry)
	require.NoError(t, err)
	assert.Contains(t, string(payload), `"credits":"1"`)
}
