package model

import (
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetLlmModelGroups 清空 options 行与内存映射，保证每条用例从「从未发布」开始。
func resetLlmModelGroups(t *testing.T) {
	t.Helper()
	require.NoError(t, DB.AutoMigrate(&Option{}))
	common.OptionMapRWMutex.Lock()
	if common.OptionMap == nil {
		common.OptionMap = map[string]string{}
	}
	common.OptionMapRWMutex.Unlock()
	wipe := func() {
		DB.Where(&Option{Key: HasnLlmModelGroupsOptionKey}).Delete(&Option{})
		require.NoError(t, applyLlmModelGroupsOption(""))
	}
	wipe()
	t.Cleanup(wipe)
}

func storedLlmModelGroupsOption(t *testing.T) string {
	t.Helper()
	var option Option
	require.NoError(t, DB.Where(&Option{Key: HasnLlmModelGroupsOptionKey}).First(&option).Error)
	return option.Value
}

func TestPutLlmModelGroupFollowsPublicationRevision(t *testing.T) {
	resetLlmModelGroups(t)

	first, err := PutLlmModelGroup("default", 3, []string{"gpt-4o", "claude-4"})
	require.Nil(t, err)
	assert.Equal(t, LlmModelGroup{PublicationRevision: 3, Models: []string{"claude-4", "gpt-4o"}}, *first)
	assert.Equal(t, map[string]bool{"claude-4": true, "gpt-4o": true}, LlmModelGroupModelSet("default"),
		"写入后本实例立即生效")
	assert.JSONEq(t, `{"default":{"publication_revision":3,"models":["claude-4","gpt-4o"]}}`, storedLlmModelGroupsOption(t))

	replay, err := PutLlmModelGroup("default", 3, []string{"claude-4", "gpt-4o"})
	require.Nil(t, err, "同世代同集合（顺序无关）是幂等重放")
	assert.Equal(t, int64(3), replay.PublicationRevision)

	_, err = PutLlmModelGroup("default", 3, []string{"gpt-4o"})
	require.NotNil(t, err)
	assert.Equal(t, http.StatusConflict, err.HTTPStatus)
	assert.Equal(t, LlmModelGroupErrorRevisionConflict, err.Code)

	_, err = PutLlmModelGroup("default", 2, []string{"gpt-4o", "dall-e-3"})
	require.NotNil(t, err)
	assert.Equal(t, http.StatusConflict, err.HTTPStatus)
	assert.Equal(t, LlmModelGroupErrorStaleRevision, err.Code)
	assert.False(t, err.Retryable)
	assert.Equal(t, map[string]bool{"claude-4": true, "gpt-4o": true}, LlmModelGroupModelSet("default"),
		"迟到的旧发布不得回滚白名单")

	narrowed, err := PutLlmModelGroup("default", 4, []string{})
	require.Nil(t, err)
	assert.Equal(t, []string{}, narrowed.Models)
	assert.Empty(t, LlmModelGroupModelSet("default"), "全停发布后组内全拒")
}

func TestPutLlmModelGroupKeepsOtherGroups(t *testing.T) {
	resetLlmModelGroups(t)

	_, err := PutLlmModelGroup("default", 1, []string{"gpt-4o"})
	require.Nil(t, err)
	_, err = PutLlmModelGroup("pro", 1, []string{"gpt-4o", "o3"})
	require.Nil(t, err)

	assert.Equal(t, map[string]bool{"gpt-4o": true}, LlmModelGroupModelSet("default"))
	assert.Equal(t, map[string]bool{"gpt-4o": true, "o3": true}, LlmModelGroupModelSet("pro"))
	assert.Empty(t, LlmModelGroupModelSet("enterprise"), "不存在的组是空集")
}

// 其他实例经 SyncOptions 读到同一行时，内存映射同样切换。
func TestLlmModelGroupsReloadFromOptionSync(t *testing.T) {
	resetLlmModelGroups(t)
	require.NoError(t, DB.Create(&Option{
		Key:   HasnLlmModelGroupsOptionKey,
		Value: `{"default":{"publication_revision":9,"models":["gpt-4o-mini"]}}`,
	}).Error)

	loadOptionsFromDatabase()

	assert.Equal(t, map[string]bool{"gpt-4o-mini": true}, LlmModelGroupModelSet("default"))
}

// 坏值不清空内存映射：清空等于把全部 lease 令牌瞬间全拒。
func TestLlmModelGroupsMalformedOptionKeepsPreviousMapping(t *testing.T) {
	resetLlmModelGroups(t)
	_, err := PutLlmModelGroup("default", 1, []string{"gpt-4o"})
	require.Nil(t, err)

	for _, malformed := range []string{
		`not-json`,
		`{"Default":{"publication_revision":2,"models":[]}}`,
		`{"default":{"publication_revision":0,"models":[]}}`,
		`{"default":{"publication_revision":2}}`,
		`{"default":{"publication_revision":2,"models":["a","a"]}}`,
	} {
		assert.Error(t, updateOptionMap(HasnLlmModelGroupsOptionKey, malformed), malformed)
		assert.Error(t, validateOptionValue(HasnLlmModelGroupsOptionKey, malformed), "管理面写入同样拒绝: %s", malformed)
		assert.Equal(t, map[string]bool{"gpt-4o": true}, LlmModelGroupModelSet("default"), malformed)
	}
}

func TestUpsertRelayLeasePersistsModelGroup(t *testing.T) {
	truncateTables(t)
	seedCreditUser(t, 19003, 3_000_000)

	spec := RelayLeaseSpec{
		ExternalLeaseId:      "generic-lease-19003",
		NewApiUserId:         19003,
		CredentialGeneration: 1,
		ModelGroup:           "default",
		ModelLimits:          []string{},
		ExpiredTime:          1_900_000_000,
		Purpose:              "generic",
		AccountingMode:       "legacy",
	}
	issued, leaseErr := UpsertRelayLease(spec)
	require.Nil(t, leaseErr)
	var stored Token
	require.NoError(t, DB.First(&stored, issued.Token.Id).Error)
	assert.Equal(t, "default", stored.ModelGroup)
	assert.False(t, stored.ModelLimitsEnabled, "generic lease 不靠令牌自身名单收窄")
	assert.Equal(t, "", stored.ModelLimits)

	spec.ModelGroup = "pro"
	spec.CredentialGeneration++
	moved, leaseErr := UpsertRelayLease(spec)
	require.Nil(t, leaseErr)
	assert.True(t, moved.Rotated, "改组必须升代并换Key")
	require.NoError(t, DB.First(&stored, issued.Token.Id).Error)
	assert.Equal(t, "pro", stored.ModelGroup)
	assert.NotEqual(t, issued.Token.Key, stored.Key)
}
