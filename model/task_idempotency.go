package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
)

// 异步任务幂等账本（LLM 网关设计 §13.3）。
//
// 图像、视频这类异步创建一旦重复提交，代价是主人被扣两次费、上游跑两次任务。
// 网络抖动下的重发又是常态，所以创建面必须自带幂等：
// 同 (Token, operation, idempotency_key, 请求摘要) 重发返回**原任务**，
// 不再次创建、不再次扣费；同 key 不同摘要返回冲突。
//
// ⚠️ request_id / trace_id / invocation_id / attempt_id 只用于观测关联，
// **禁止**作为幂等键或去重依据——它们每次重发都会变，用它们去重等于没有去重。

// 账本状态。
const (
	// TaskIdempotencyStatusProcessing 已占位、任务尚未落库。
	// 它借唯一索引把同一把键的并发请求串行化。
	TaskIdempotencyStatusProcessing = "processing"
	// TaskIdempotencyStatusSucceeded 任务已落库，记录了 task_id。
	TaskIdempotencyStatusSucceeded = "succeeded"
	// TaskIdempotencyStatusFailed 终局失败，且**无法确证上游没有收到请求**。
	// 这一格存在的唯一理由：同键重发时把上次的失败原样还回去，
	// 而不是再发一次可能已经在上游跑着的任务。
	TaskIdempotencyStatusFailed = "failed"
)

// 幂等错误码。冲突沿用本仓积分履约账本已有的 idempotency_conflict 命名，
// 同一个概念不另起名字。
const (
	TaskIdempotencyErrorConflict         = "idempotency_conflict"
	TaskIdempotencyErrorInProgress       = "idempotency_request_in_progress"
	TaskIdempotencyErrorInvalidKey       = "invalid_idempotency_key"
	TaskIdempotencyErrorStoreUnavailable = "idempotency_store_unavailable"
)

// MaxTaskIdempotencyKeyLength 是可接受的幂等键长度上限。
// 超长的键既撑不进列宽，也说明调用方把别的东西塞进了这个位置。
const MaxTaskIdempotencyKeyLength = 128

// taskIdempotencyInFlightTTL 是 processing 行的接管时限。
//
// 进程在占位与落库之间崩溃会留下一行永远 processing 的记录；没有接管机制，
// 那把键就此报废，主人再也提交不了同一个请求。超过这个时限即可被新请求接管。
const taskIdempotencyInFlightTTL = 10 * time.Minute

// TaskIdempotencyRecord 是一把幂等键的账本行。
//
// 唯一索引落在派生列 ScopeHash 上，而不是 (TokenId, Operation, IdempotencyKey)
// 复合索引：MySQL 5.7.8 的 InnoDB COMPACT 行格式下单个索引最长 767 字节，
// utf8mb4 的 varchar(128)+varchar(128) 复合索引会直接超限建不出来。
// 明文三列照样保留，供人排查。
type TaskIdempotencyRecord struct {
	Id int64 `json:"id" gorm:"primaryKey;autoIncrement"`

	// ScopeHash = sha256(token_id | operation | idempotency_key)，幂等键的唯一约束载体。
	ScopeHash string `json:"scope_hash" gorm:"type:varchar(64);uniqueIndex"`

	TokenId        int    `json:"token_id" gorm:"index"`
	Operation      string `json:"operation" gorm:"type:varchar(128)"`
	IdempotencyKey string `json:"idempotency_key" gorm:"type:varchar(128)"`

	// RequestDigest 是请求摘要。同 ScopeHash 但摘要不同即冲突。
	RequestDigest string `json:"request_digest" gorm:"type:varchar(64)"`

	// TaskID 是成功创建的任务 ID，与 tasks.task_id 同名同义。
	TaskID string `json:"task_id" gorm:"type:varchar(191);index"`

	Status string `json:"status" gorm:"type:varchar(16);index"`

	// 终局失败的回放材料。只有 status=failed 时有值。
	FailureCode       string `json:"failure_code" gorm:"type:varchar(64);default:''"`
	FailureMessage    string `json:"failure_message" gorm:"type:varchar(512);default:''"`
	FailureStatusCode int    `json:"failure_status_code"`

	CreatedAt int64 `json:"created_at" gorm:"bigint;index"`
	UpdatedAt int64 `json:"updated_at" gorm:"bigint;index"`
}

func (TaskIdempotencyRecord) TableName() string {
	return "task_idempotency_records"
}

func (r *TaskIdempotencyRecord) BeforeCreate(_ *gorm.DB) error {
	now := common.GetTimestamp()
	r.CreatedAt = now
	r.UpdatedAt = now
	return nil
}

// TaskIdempotencyScope 是一把幂等键的四元组身份。
type TaskIdempotencyScope struct {
	TokenId        int
	Operation      string
	IdempotencyKey string
	RequestDigest  string
}

// ScopeHash 计算唯一约束载体。请求摘要**不**参与，因为「同键不同摘要」
// 必须撞上同一行才能被判成冲突；摘要参与了就会变成两把互不相干的键。
func (s TaskIdempotencyScope) ScopeHash() string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%d\n%s\n%s", s.TokenId, s.Operation, s.IdempotencyKey)))
	return hex.EncodeToString(digest[:])
}

// TaskIdempotencyError 携带 HTTP 语义的幂等错误。
type TaskIdempotencyError struct {
	Code       string
	Message    string
	HTTPStatus int
}

func (e *TaskIdempotencyError) Error() string {
	if e == nil {
		return ""
	}
	return e.Code + ": " + e.Message
}

func taskIdempotencyError(status int, code, format string, args ...any) *TaskIdempotencyError {
	return &TaskIdempotencyError{Code: code, Message: fmt.Sprintf(format, args...), HTTPStatus: status}
}

// TaskIdempotencyReplay 是命中既有终局记录时要原样还回去的东西。
type TaskIdempotencyReplay struct {
	// TaskID 非空表示命中已成功的原任务。
	TaskID string
	// FailureCode 非空表示命中已记录的终局失败。
	FailureCode       string
	FailureMessage    string
	FailureStatusCode int
}

// ValidateTaskIdempotencyKey 校验幂等键的形状。
// 只接受可见 ASCII，避免控制字符与不可见字符让同一把键在日志里长成两样。
func ValidateTaskIdempotencyKey(key string) (string, *TaskIdempotencyError) {
	trimmed := strings.TrimSpace(key)
	if trimmed == "" {
		return "", taskIdempotencyError(http.StatusBadRequest, TaskIdempotencyErrorInvalidKey, "idempotency key must not be empty")
	}
	if len(trimmed) > MaxTaskIdempotencyKeyLength {
		return "", taskIdempotencyError(http.StatusBadRequest, TaskIdempotencyErrorInvalidKey,
			"idempotency key must be at most %d characters", MaxTaskIdempotencyKeyLength)
	}
	for _, r := range trimmed {
		if r < 0x21 || r > 0x7e {
			return "", taskIdempotencyError(http.StatusBadRequest, TaskIdempotencyErrorInvalidKey,
				"idempotency key must contain only printable ASCII characters")
		}
	}
	return trimmed, nil
}

// AcquireTaskIdempotency 尝试占用一把幂等键。返回三态：
//
//   - (record, nil, nil)：抢到了，调用方继续创建任务，随后必须调用
//     CompleteTaskIdempotency 或 FailTaskIdempotency/ReleaseTaskIdempotency 收口；
//   - (nil, replay, nil)：同键同摘要已有终局记录，调用方直接把 replay 还给客户端，
//     不得再次创建、不得再次扣费；
//   - (nil, nil, err)：同键不同摘要（冲突）、同键仍在途，或存储故障。
func AcquireTaskIdempotency(scope TaskIdempotencyScope) (*TaskIdempotencyRecord, *TaskIdempotencyReplay, *TaskIdempotencyError) {
	scopeHash := scope.ScopeHash()
	record := &TaskIdempotencyRecord{
		ScopeHash:      scopeHash,
		TokenId:        scope.TokenId,
		Operation:      scope.Operation,
		IdempotencyKey: scope.IdempotencyKey,
		RequestDigest:  scope.RequestDigest,
		Status:         TaskIdempotencyStatusProcessing,
	}
	// 先抢插：唯一索引是唯一可靠的互斥点。先查后插在并发下会双双查不到、双双插入。
	if err := DB.Create(record).Error; err == nil {
		return record, nil, nil
	} else if !isTaskIdempotencyDuplicate(err) {
		return nil, nil, taskIdempotencyError(http.StatusInternalServerError, TaskIdempotencyErrorStoreUnavailable,
			"failed to reserve idempotency key: %s", err.Error())
	}

	var existing TaskIdempotencyRecord
	found := DB.Where("scope_hash = ?", scopeHash).Limit(1).Find(&existing)
	if found.Error != nil {
		return nil, nil, taskIdempotencyError(http.StatusInternalServerError, TaskIdempotencyErrorStoreUnavailable,
			"failed to load idempotency record: %s", found.Error.Error())
	}
	if found.RowsAffected == 0 {
		// 抢插撞了唯一索引却又查不到：只可能是对方事务回滚。让调用方重发。
		return nil, nil, taskIdempotencyError(http.StatusConflict, TaskIdempotencyErrorInProgress,
			"idempotency key %q is being processed concurrently", scope.IdempotencyKey)
	}
	if existing.RequestDigest != scope.RequestDigest {
		return nil, nil, taskIdempotencyError(http.StatusConflict, TaskIdempotencyErrorConflict,
			"idempotency key %q was already used with a different request", scope.IdempotencyKey)
	}

	switch existing.Status {
	case TaskIdempotencyStatusSucceeded:
		return nil, &TaskIdempotencyReplay{TaskID: existing.TaskID}, nil
	case TaskIdempotencyStatusFailed:
		return nil, &TaskIdempotencyReplay{
			FailureCode:       existing.FailureCode,
			FailureMessage:    existing.FailureMessage,
			FailureStatusCode: existing.FailureStatusCode,
		}, nil
	}

	// 仍在途。超过时限的按「上一个进程已经死了」接管，否则拒绝并发重发。
	staleBefore := common.GetTimestamp() - int64(taskIdempotencyInFlightTTL.Seconds())
	if existing.UpdatedAt > staleBefore {
		return nil, nil, taskIdempotencyError(http.StatusConflict, TaskIdempotencyErrorInProgress,
			"idempotency key %q is being processed concurrently", scope.IdempotencyKey)
	}
	now := common.GetTimestamp()
	// CAS 接管：条件里带上 status 与 updated_at，两个请求同时接管只有一个能成。
	takeover := DB.Model(&TaskIdempotencyRecord{}).
		Where("id = ? AND status = ? AND updated_at = ?", existing.Id, TaskIdempotencyStatusProcessing, existing.UpdatedAt).
		Updates(map[string]any{"updated_at": now})
	if takeover.Error != nil {
		return nil, nil, taskIdempotencyError(http.StatusInternalServerError, TaskIdempotencyErrorStoreUnavailable,
			"failed to take over stale idempotency record: %s", takeover.Error.Error())
	}
	if takeover.RowsAffected == 0 {
		return nil, nil, taskIdempotencyError(http.StatusConflict, TaskIdempotencyErrorInProgress,
			"idempotency key %q is being processed concurrently", scope.IdempotencyKey)
	}
	existing.UpdatedAt = now
	return &existing, nil, nil
}

// CompleteTaskIdempotency 在任务已经落库之后把键钉成 succeeded。
func CompleteTaskIdempotency(record *TaskIdempotencyRecord, taskID string) error {
	if record == nil {
		return nil
	}
	now := common.GetTimestamp()
	result := DB.Model(&TaskIdempotencyRecord{}).
		Where("id = ? AND status = ?", record.Id, TaskIdempotencyStatusProcessing).
		Updates(map[string]any{
			"status":     TaskIdempotencyStatusSucceeded,
			"task_id":    taskID,
			"updated_at": now,
		})
	if result.Error != nil {
		return result.Error
	}
	record.Status = TaskIdempotencyStatusSucceeded
	record.TaskID = taskID
	record.UpdatedAt = now
	return nil
}

// FailTaskIdempotency 把键钉成终局失败，供同键重发原样回放。
//
// 只在**无法确证上游没有收到请求**时使用（dispatch_state 是 dispatched 或
// unknown）。此时放开这把键等于允许再发一次可能已经在上游跑着的任务。
func FailTaskIdempotency(record *TaskIdempotencyRecord, code, message string, statusCode int) error {
	if record == nil {
		return nil
	}
	if len(message) > 512 {
		message = message[:512]
	}
	now := common.GetTimestamp()
	result := DB.Model(&TaskIdempotencyRecord{}).
		Where("id = ? AND status = ?", record.Id, TaskIdempotencyStatusProcessing).
		Updates(map[string]any{
			"status":              TaskIdempotencyStatusFailed,
			"failure_code":        code,
			"failure_message":     message,
			"failure_status_code": statusCode,
			"updated_at":          now,
		})
	if result.Error != nil {
		return result.Error
	}
	record.Status = TaskIdempotencyStatusFailed
	record.FailureCode = code
	record.FailureMessage = message
	record.FailureStatusCode = statusCode
	record.UpdatedAt = now
	return nil
}

// ReleaseTaskIdempotency 删掉占位行，把键还给调用方。
//
// 只在**确证上游一个字节都没收到**时使用（dispatch_state = not_dispatched）：
// 既然什么都没发生，同一把键理应还能再试一次。
func ReleaseTaskIdempotency(record *TaskIdempotencyRecord) error {
	if record == nil {
		return nil
	}
	return DB.Where("id = ? AND status = ?", record.Id, TaskIdempotencyStatusProcessing).
		Delete(&TaskIdempotencyRecord{}).Error
}

// isTaskIdempotencyDuplicate 判断错误是不是唯一索引冲突。
// GORM 从 v1.24 起提供跨方言的 ErrDuplicatedKey，但它依赖驱动的
// TranslateError 开关；三库里任何一库没打开都会退化成裸驱动错误，
// 所以再补一层字符串兜底。
func isTaskIdempotencyDuplicate(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"unique constraint",   // PostgreSQL
		"duplicate entry",     // MySQL
		"unique violation",    // PostgreSQL（部分驱动措辞）
		"constraint failed",   // SQLite
		"duplicate key value", // PostgreSQL
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}
