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

func TestNormalizeRelayLeaseAccountingGenericForms(t *testing.T) {
	// 省略与显式 generic/legacy 等价，统一归一为 generic/legacy
	for _, form := range [][2]string{{"", ""}, {"generic", "legacy"}} {
		purpose, mode, err := NormalizeRelayLeaseAccounting(form[0], form[1])
		require.NoError(t, err)
		require.Equal(t, "generic", purpose)
		require.Equal(t, "legacy", mode)
	}
	// 半显式、generic 配 synchronous、音频配 legacy 一律拒绝
	for _, form := range [][2]string{{"generic", ""}, {"", "legacy"}, {"generic", "synchronous"}, {"audio.tts", "legacy"}, {"audio.unknown", "synchronous"}} {
		_, _, err := NormalizeRelayLeaseAccounting(form[0], form[1])
		require.Error(t, err, "%v", form)
	}
	purpose, mode, err := NormalizeRelayLeaseAccounting("audio.transcribe", "synchronous")
	require.NoError(t, err)
	require.Equal(t, "audio.transcribe", purpose)
	require.Equal(t, "synchronous", mode)
}

func TestUpsertRelayLeaseExplicitGenericStoresGenericLegacy(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 19002, 3_000_000)

	for _, form := range [][2]string{{"generic", "legacy"}, {"", ""}} {
		spec := RelayLeaseSpec{
			ExternalLeaseId:      "generic-lease-19002-" + form[0],
			NewApiUserId:         19002,
			CredentialGeneration: 1,
			ModelLimits:          []string{"gpt-4o-mini"},
			ExpiredTime:          time.Now().Add(time.Hour).Unix(),
			Purpose:              form[0],
			AccountingMode:       form[1],
		}
		issued, leaseErr := UpsertRelayLease(spec)
		require.Nil(t, leaseErr)
		var stored Token
		require.NoError(t, DB.First(&stored, issued.Token.Id).Error)
		require.Equal(t, "generic", stored.Purpose)
		require.Equal(t, "legacy", stored.AccountingMode)
	}
}
