package model

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
)

// LLM 模型组（LLM 网关设计 §8.2.2）。
//
// # 为什么白名单挂在组上，而不是逐令牌写
//
// 旧做法每次发布都要把全部 Relay lease 的 model_limits 逐条改写一遍，代价随令牌数增长。
// 模型组把「一组可调用模型」收成一个名字：令牌只记自己属于哪个组（tokens.model_group），
// 发布只改组 → 模型映射，一次写入、与用户数无关。本期只有一个组 default，将来对应订阅档。
//
// # 存储
//
// 映射存在既有 options 表的一条键里，读路径走内存映射。写入后本实例立即生效，其他实例经
// 既有 options 同步（SYNC_FREQUENCY）生效——这就是收紧型发布在多实例下的生效时延上界。

const (
	// HasnLlmModelGroupsOptionKey 是模型组映射在 options 表里的键。
	HasnLlmModelGroupsOptionKey = "HasnLlmModelGroups"

	// LlmModelGroupErrorRevisionConflict：同一发布世代却给出不同的模型集合。
	LlmModelGroupErrorRevisionConflict = "model_group_revision_conflict"
	// LlmModelGroupErrorStaleRevision：存量世代已比请求新，迟到的旧发布不得回滚白名单。
	LlmModelGroupErrorStaleRevision = "stale_publication_revision"

	// maxLlmModelNames 是一份模型名单的条目上限（组映射与 lease 收窄名单共用）。
	maxLlmModelNames = 1000
	// maxLlmModelNameBytes 是单个模型名的 UTF-8 字节上限。
	maxLlmModelNameBytes = 128
)

var llmModelGroupNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// LlmModelGroup 是一个模型组的存量：照着哪一版发布写的，以及可调用的模型。
type LlmModelGroup struct {
	PublicationRevision int64    `json:"publication_revision"`
	Models              []string `json:"models"`
}

var llmModelGroups = struct {
	sync.RWMutex
	byName map[string]LlmModelGroup
}{byName: map[string]LlmModelGroup{}}

// llmModelGroupWriteLock 串行化本进程内的组写入；跨实例由数据库行锁与世代比较兜底。
var llmModelGroupWriteLock sync.Mutex

// IsValidLlmModelGroupName 判定组名是否落在约定词法内。
func IsValidLlmModelGroupName(name string) bool {
	return llmModelGroupNamePattern.MatchString(name)
}

// NormalizeLlmModelNames 校验一份模型名单（允许为空），返回去重校验后的原序副本。
//
// 逗号是硬约束：lease 收窄名单按逗号拼进 tokens.model_limits，名字里带逗号读回来会裂成两个；
// 组映射虽然存 JSON，仍沿用同一条规则，保证同一个模型名在两处都能表达。
func NormalizeLlmModelNames(models []string) ([]string, error) {
	if len(models) > maxLlmModelNames {
		return nil, fmt.Errorf("model list exceeds %d entries", maxLlmModelNames)
	}
	seen := make(map[string]struct{}, len(models))
	normalized := make([]string, 0, len(models))
	for _, name := range models {
		if name == "" || strings.TrimSpace(name) != name {
			return nil, errors.New("model names must be non-empty and free of surrounding whitespace")
		}
		if len(name) > maxLlmModelNameBytes {
			return nil, fmt.Errorf("model name exceeds %d bytes", maxLlmModelNameBytes)
		}
		if strings.Contains(name, ",") {
			return nil, errors.New("model names must not contain a comma")
		}
		if _, duplicated := seen[name]; duplicated {
			return nil, fmt.Errorf("model list contains duplicate entry %q", name)
		}
		seen[name] = struct{}{}
		normalized = append(normalized, name)
	}
	return normalized, nil
}

// parseLlmModelGroups 解析 options 里的映射；任一组名、世代或名单不合法都整体拒绝。
func parseLlmModelGroups(value string) (map[string]LlmModelGroup, error) {
	groups := map[string]LlmModelGroup{}
	if strings.TrimSpace(value) == "" {
		return groups, nil
	}
	if err := common.UnmarshalJsonStr(value, &groups); err != nil {
		return nil, fmt.Errorf("%s is not a valid JSON object: %w", HasnLlmModelGroupsOptionKey, err)
	}
	for name, group := range groups {
		if !IsValidLlmModelGroupName(name) {
			return nil, fmt.Errorf("%s has an invalid model group name %q", HasnLlmModelGroupsOptionKey, name)
		}
		if group.PublicationRevision < 1 {
			return nil, fmt.Errorf("%s group %q has a non-positive publication_revision", HasnLlmModelGroupsOptionKey, name)
		}
		if group.Models == nil {
			return nil, fmt.Errorf("%s group %q is missing models", HasnLlmModelGroupsOptionKey, name)
		}
		if _, err := NormalizeLlmModelNames(group.Models); err != nil {
			return nil, fmt.Errorf("%s group %q: %w", HasnLlmModelGroupsOptionKey, name, err)
		}
	}
	return groups, nil
}

// applyLlmModelGroupsOption 由 updateOptionMap 调用（本实例写入与 SyncOptions 同步都经过它）。
//
// 解析失败时保留内存里的旧映射并返回错误：坏值来自人工改库或截断，此时清空等于把全部
// lease 令牌瞬间全拒，而旧映射至少是上一份通过校验的发布结果。
func applyLlmModelGroupsOption(value string) error {
	groups, err := parseLlmModelGroups(value)
	if err != nil {
		return err
	}
	llmModelGroups.Lock()
	llmModelGroups.byName = groups
	llmModelGroups.Unlock()
	return nil
}

// LlmModelGroupModelSet 返回该组可调用模型的集合副本；组不存在时返回空集（全拒）。
func LlmModelGroupModelSet(name string) map[string]bool {
	llmModelGroups.RLock()
	defer llmModelGroups.RUnlock()
	group := llmModelGroups.byName[name]
	allowed := make(map[string]bool, len(group.Models))
	for _, model := range group.Models {
		allowed[model] = true
	}
	return allowed
}

// PutLlmModelGroup 按发布世代单调写入一个模型组，返回写后存量。
//
//   - 组不存在或存量世代更小 → 覆盖；
//   - 世代相等且集合相同     → 幂等，原样返回；
//   - 世代相等但集合不同     → 409 model_group_revision_conflict；
//   - 存量世代更大           → 409 stale_publication_revision。
func PutLlmModelGroup(name string, publicationRevision int64, models []string) (*LlmModelGroup, *RelayLeaseError) {
	requested := slices.Clone(models)
	sort.Strings(requested)

	llmModelGroupWriteLock.Lock()
	defer llmModelGroupWriteLock.Unlock()

	var (
		result  LlmModelGroup
		encoded string
		changed bool
		refusal *RelayLeaseError
	)
	err := DB.Transaction(func(tx *gorm.DB) error {
		var rows []Option
		if err := lockForUpdate(tx).Where(&Option{Key: HasnLlmModelGroupsOptionKey}).Find(&rows).Error; err != nil {
			return err
		}
		stored := ""
		if len(rows) > 0 {
			stored = rows[0].Value
		}
		groups, parseErr := parseLlmModelGroups(stored)
		if parseErr != nil {
			return parseErr
		}
		current, exists := groups[name]
		if exists && current.PublicationRevision > publicationRevision {
			refusal = relayLeaseError(http.StatusConflict, LlmModelGroupErrorStaleRevision, false,
				"model group %s is already at publication_revision %d", name, current.PublicationRevision)
			return nil
		}
		if exists && current.PublicationRevision == publicationRevision {
			currentModels := slices.Clone(current.Models)
			sort.Strings(currentModels)
			if !slices.Equal(currentModels, requested) {
				refusal = relayLeaseError(http.StatusConflict, LlmModelGroupErrorRevisionConflict, false,
					"model group %s already holds a different model set at publication_revision %d", name, publicationRevision)
				return nil
			}
			result = current
			return nil
		}
		result = LlmModelGroup{PublicationRevision: publicationRevision, Models: requested}
		groups[name] = result
		payload, marshalErr := common.Marshal(groups)
		if marshalErr != nil {
			return marshalErr
		}
		encoded, changed = string(payload), true
		if len(rows) == 0 {
			return tx.Create(&Option{Key: HasnLlmModelGroupsOptionKey, Value: encoded}).Error
		}
		return tx.Model(&Option{Key: HasnLlmModelGroupsOptionKey}).Update("value", encoded).Error
	})
	if err != nil {
		common.SysError("模型组写入失败: " + err.Error())
		return nil, relayLeaseError(http.StatusServiceUnavailable, RelayLeaseErrorStorageUnavailable, true,
			"failed to persist the model group")
	}
	if refusal != nil {
		return nil, refusal
	}
	if changed {
		// 提交之后才切本实例内存：提交失败时内存绝不领先于数据库。
		if mapErr := updateOptionMap(HasnLlmModelGroupsOptionKey, encoded); mapErr != nil {
			common.SysError("模型组已落库但本实例内存刷新失败，等待下次同步: " + mapErr.Error())
		}
	}
	return &result, nil
}
