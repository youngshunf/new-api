package middleware

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// setupModelGroups 用真实 options 写入口装好模型组映射（设计 §8.2.2）。
func setupModelGroups(t *testing.T, value string) {
	t.Helper()
	previousDB := model.DB
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Option{}))
	model.DB = db
	common.OptionMapRWMutex.Lock()
	if common.OptionMap == nil {
		common.OptionMap = map[string]string{}
	}
	common.OptionMapRWMutex.Unlock()
	require.NoError(t, model.UpdateOption(model.HasnLlmModelGroupsOptionKey, value))
	t.Cleanup(func() {
		require.NoError(t, model.UpdateOption(model.HasnLlmModelGroupsOptionKey, ""))
		model.DB = previousDB
	})
}

func tokenModelLimitContext(t *testing.T, token *model.Token) (bool, map[string]bool) {
	t.Helper()
	ctx := newTokenAutoGroupsContext()
	require.NoError(t, SetupContextForToken(ctx, token))
	enabled := common.GetContextKeyBool(ctx, constant.ContextKeyTokenModelLimitEnabled)
	value, ok := common.GetContextKey(ctx, constant.ContextKeyTokenModelLimit)
	if !ok {
		return enabled, nil
	}
	return enabled, value.(map[string]bool)
}

func TestSetupContextForTokenUsesModelGroup(t *testing.T) {
	setupModelGroups(t, `{"default":{"publication_revision":5,"models":["gpt-4o","claude-4"]}}`)

	enabled, allowed := tokenModelLimitContext(t, &model.Token{Id: 1, UserId: 2, ModelGroup: "default"})

	assert.True(t, enabled)
	assert.Equal(t, map[string]bool{"gpt-4o": true, "claude-4": true}, allowed)
}

// 独立音频 lease：组 ∩ 令牌自身收窄名单；收窄名单里有、组里没有的模型同样不可调。
func TestSetupContextForTokenIntersectsModelGroupWithTokenLimits(t *testing.T) {
	setupModelGroups(t, `{"default":{"publication_revision":5,"models":["gpt-4o","tts-1"]}}`)

	enabled, allowed := tokenModelLimitContext(t, &model.Token{
		Id: 1, UserId: 2, ModelGroup: "default", ModelLimitsEnabled: true, ModelLimits: "tts-1,whisper-1",
	})

	assert.True(t, enabled)
	assert.Equal(t, map[string]bool{"tts-1": true}, allowed)
}

func TestSetupContextForTokenUnknownModelGroupDeniesEverything(t *testing.T) {
	setupModelGroups(t, `{"default":{"publication_revision":5,"models":["gpt-4o"]}}`)

	enabled, allowed := tokenModelLimitContext(t, &model.Token{Id: 1, UserId: 2, ModelGroup: "pro"})

	assert.True(t, enabled, "组不存在必须走限制分支，而不是退回不限制")
	assert.Equal(t, map[string]bool{}, allowed)
}

// 上游用户自建令牌（model_group 为空）行为不变。
func TestSetupContextForTokenWithoutModelGroupKeepsUpstreamBehavior(t *testing.T) {
	setupModelGroups(t, `{"default":{"publication_revision":5,"models":["gpt-4o"]}}`)

	enabled, _ := tokenModelLimitContext(t, &model.Token{Id: 1, UserId: 2})
	assert.False(t, enabled)

	enabled, allowed := tokenModelLimitContext(t, &model.Token{
		Id: 1, UserId: 2, ModelLimitsEnabled: true, ModelLimits: "o3",
	})
	assert.True(t, enabled)
	assert.Equal(t, map[string]bool{"o3": true}, allowed)
}
