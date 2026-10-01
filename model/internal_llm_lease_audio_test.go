package model

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUpsertRelayLeasePersistsManagedAudioIdentity(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 19001, 3_000_000)

	spec := RelayLeaseSpec{
		ExternalLeaseId:      "audio-lease-19001",
		NewApiUserId:         19001,
		CredentialGeneration: 1,
		ModelLimits:          []string{"tts-1"},
		ExpiredTime:          time.Now().Add(time.Hour).Unix(),
		Purpose:              "audio.tts",
		AccountingMode:       "synchronous",
	}
	issued, leaseErr := UpsertRelayLease(spec)
	require.Nil(t, leaseErr)
	require.NotNil(t, issued)
	require.Equal(t, "audio.tts", issued.Token.Purpose)
	require.Equal(t, "synchronous", issued.Token.AccountingMode)
	require.NotNil(t, issued.Token.ExternalLeaseId)
	require.Equal(t, spec.ExternalLeaseId, *issued.Token.ExternalLeaseId)

	var stored Token
	require.NoError(t, DB.First(&stored, issued.Token.Id).Error)
	require.Equal(t, "audio.tts", stored.Purpose)
	require.Equal(t, "synchronous", stored.AccountingMode)
	require.NotNil(t, stored.ExternalLeaseId)
	require.Equal(t, spec.ExternalLeaseId, *stored.ExternalLeaseId)

	replay, replayErr := UpsertRelayLease(spec)
	require.Nil(t, replayErr)
	require.False(t, replay.Created)
	require.False(t, replay.Rotated)
	require.Equal(t, issued.Token.Id, replay.Token.Id)
}
