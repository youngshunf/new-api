package model

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"regexp"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	InternalAccountErrorInvalidRequest   = "invalid_request"
	InternalAccountErrorUserNotFound     = "newapi_user_not_found"
	InternalAccountErrorStoreUnavailable = "store_unavailable"
	// 冲突沿用积分履约账本已有的 idempotency_conflict 命名。
	InternalAccountErrorIdempotencyConflict = "idempotency_conflict"
	InternalAccountWorkspaceKindPersonal    = "personal"
	InternalAccountWorkspaceKindEnterprise  = "enterprise"
	internalAccountUsernamePrefix           = "iap-"
	internalAccountProvisionKeyMaxLength    = 64
)

var (
	errInternalAccountProvisionRace = errors.New("internal account provision already exists")
	internalAccountWorkspaceKeyRe   = regexp.MustCompile(`^workspace_[A-Za-z0-9_-]{3,96}$`)
)

// InternalAccountProvision 记录云端开户键到 NewAPI 用户的映射。
// workspace_key / workspace_kind 只用于对账与运营查询，不参与鉴权、扣费与限额；
// 旧 core 开户不带这两项时存 NULL，首次带值时写入，之后不变。
// 企业一个空间有多个席位账户，workspace_key 不建唯一约束。
type InternalAccountProvision struct {
	Id            int     `json:"id"`
	ProvisionKey  string  `json:"provision_key" gorm:"type:varchar(64);uniqueIndex;not null"`
	UserId        int     `json:"user_id" gorm:"column:user_id;index;not null"`
	WorkspaceKey  *string `json:"workspace_key" gorm:"column:workspace_key;type:varchar(106)"`
	WorkspaceKind *string `json:"workspace_kind" gorm:"column:workspace_kind;type:varchar(16)"`
	CreatedTime   int64   `json:"created_time" gorm:"autoCreateTime;column:created_time"`
}

// InternalAccountWorkspace 是开户请求声明的账户所属空间。
type InternalAccountWorkspace struct {
	Key  string
	Kind string
}

// ValidateInternalAccountWorkspace 校验开户请求里的空间两项：要么都给、要么都不给。
// 都不给返回 nil。
func ValidateInternalAccountWorkspace(workspaceKey *string, workspaceKind *string) (*InternalAccountWorkspace, *InternalAccountError) {
	if workspaceKey == nil && workspaceKind == nil {
		return nil, nil
	}
	if workspaceKey == nil || workspaceKind == nil {
		return nil, internalAccountError(http.StatusBadRequest, InternalAccountErrorInvalidRequest, false,
			"workspace_key and workspace_kind must be provided together")
	}
	if !internalAccountWorkspaceKeyRe.MatchString(*workspaceKey) {
		return nil, internalAccountError(http.StatusBadRequest, InternalAccountErrorInvalidRequest, false,
			"workspace_key must match ^workspace_[A-Za-z0-9_-]{3,96}$")
	}
	if *workspaceKind != InternalAccountWorkspaceKindPersonal && *workspaceKind != InternalAccountWorkspaceKindEnterprise {
		return nil, internalAccountError(http.StatusBadRequest, InternalAccountErrorInvalidRequest, false,
			"workspace_kind must be personal or enterprise")
	}
	return &InternalAccountWorkspace{Key: *workspaceKey, Kind: *workspaceKind}, nil
}

type InternalAccountError struct {
	HTTPStatus int
	Code       string
	Message    string
	Retryable  bool
	Cause      string `json:"-"`
}

func (e *InternalAccountError) Error() string {
	return e.Code + ": " + e.Message
}

func internalAccountError(status int, code string, retryable bool, format string, args ...any) *InternalAccountError {
	return &InternalAccountError{HTTPStatus: status, Code: code, Retryable: retryable, Message: fmt.Sprintf(format, args...)}
}

func internalAccountStorageError(cause error) *InternalAccountError {
	err := internalAccountError(http.StatusServiceUnavailable, InternalAccountErrorStoreUnavailable, true,
		"account store unavailable")
	if cause != nil {
		err.Cause = cause.Error()
	}
	return err
}

func ValidateInternalProvisionKey(provisionKey string) (string, *InternalAccountError) {
	if provisionKey == "" || len(provisionKey) > internalAccountProvisionKeyMaxLength {
		return "", internalAccountError(http.StatusBadRequest, InternalAccountErrorInvalidRequest, false,
			"provision_key must be a non-empty string of at most 64 chars")
	}
	return provisionKey, nil
}

// ProvisionInternalAccount 按 provision_key 幂等开户；workspace 为 nil 表示请求未声明空间。
func ProvisionInternalAccount(provisionKey string, workspace *InternalAccountWorkspace) (int, *InternalAccountError) {
	validatedKey, validationErr := ValidateInternalProvisionKey(provisionKey)
	if validationErr != nil {
		return 0, validationErr
	}

	var userId int
	var conflictErr *InternalAccountError
	err := DB.Transaction(func(tx *gorm.DB) error {
		var existing InternalAccountProvision
		found := tx.Where("provision_key = ?", validatedKey).Limit(1).Find(&existing)
		if found.Error != nil {
			return found.Error
		}
		if found.RowsAffected > 0 {
			userId = existing.UserId
			conflict, reconcileErr := reconcileInternalAccountWorkspace(tx, existing, workspace)
			conflictErr = conflict
			return reconcileErr
		}

		user := User{
			Username:    internalAccountUsername(validatedKey),
			Password:    "",
			DisplayName: "Cloud Account",
			Role:        common.RoleCommonUser,
			Status:      common.UserStatusEnabled,
			Group:       "default",
			Quota:       0,
			AffCode:     internalAccountAffCode(validatedKey),
		}
		createdUser := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&user)
		if createdUser.Error != nil {
			return createdUser.Error
		}
		if createdUser.RowsAffected == 0 {
			return errInternalAccountProvisionRace
		}
		record := InternalAccountProvision{ProvisionKey: validatedKey, UserId: user.Id}
		if workspace != nil {
			record.WorkspaceKey = &workspace.Key
			record.WorkspaceKind = &workspace.Kind
		}
		createdRecord := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&record)
		if createdRecord.Error != nil {
			return createdRecord.Error
		}
		if createdRecord.RowsAffected == 0 {
			return errInternalAccountProvisionRace
		}
		userId = user.Id
		return nil
	})
	if errors.Is(err, errInternalAccountProvisionRace) {
		var existing InternalAccountProvision
		query := DB.Where("provision_key = ?", validatedKey).Limit(1).Find(&existing)
		err = query.Error
		if err == nil && query.RowsAffected == 0 {
			err = errInternalAccountProvisionRace
		}
		if err == nil {
			userId = existing.UserId
			conflictErr, err = reconcileInternalAccountWorkspace(DB, existing, workspace)
		}
	}
	if err != nil {
		return 0, internalAccountStorageError(err)
	}
	if conflictErr != nil {
		return 0, conflictErr
	}
	return userId, nil
}

// reconcileInternalAccountWorkspace 处理同 provision_key 重放时的空间两项：
// 请求未声明 ⇒ 原样返回；已存为空 ⇒ 补写一次（兼容旧 core 先开的户）；已存不同 ⇒ 409。
func reconcileInternalAccountWorkspace(tx *gorm.DB, existing InternalAccountProvision, workspace *InternalAccountWorkspace) (*InternalAccountError, error) {
	if workspace == nil {
		return nil, nil
	}
	if existing.WorkspaceKey == nil && existing.WorkspaceKind == nil {
		// 只在仍为空时写入；并发补写不同值时由下面的重读判冲突。
		backfill := tx.Model(&InternalAccountProvision{}).
			Where("id = ? AND workspace_key IS NULL AND workspace_kind IS NULL", existing.Id).
			Updates(map[string]any{"workspace_key": workspace.Key, "workspace_kind": workspace.Kind})
		if backfill.Error != nil {
			return nil, backfill.Error
		}
		if backfill.RowsAffected > 0 {
			return nil, nil
		}
		reread := tx.Where("id = ?", existing.Id).Limit(1).Find(&existing)
		if reread.Error != nil {
			return nil, reread.Error
		}
	}
	if existing.WorkspaceKey != nil && existing.WorkspaceKind != nil &&
		*existing.WorkspaceKey == workspace.Key && *existing.WorkspaceKind == workspace.Kind {
		return nil, nil
	}
	return internalAccountError(http.StatusConflict, InternalAccountErrorIdempotencyConflict, false,
		"provision_key %s was already provisioned for a different workspace", existing.ProvisionKey), nil
}

func internalAccountProvisionDigest(provisionKey string) string {
	sum := sha256.Sum256([]byte(provisionKey))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func internalAccountUsername(provisionKey string) string {
	return internalAccountUsernamePrefix + internalAccountProvisionDigest(provisionKey)[:16]
}

func internalAccountAffCode(provisionKey string) string {
	return "iap" + internalAccountProvisionDigest(provisionKey)[:29]
}

func GetInternalAccountStatus(userId int) (int, *InternalAccountError) {
	if userId <= 0 {
		return 0, internalAccountError(http.StatusBadRequest, InternalAccountErrorInvalidRequest, false,
			"newapi_user_id must be a positive integer")
	}
	var user User
	query := DB.Select("id", "status").Where("id = ?", userId).Limit(1).Find(&user)
	if query.Error != nil {
		return 0, internalAccountStorageError(query.Error)
	}
	if query.RowsAffected == 0 {
		return 0, internalAccountError(http.StatusNotFound, InternalAccountErrorUserNotFound, false,
			"newapi user %d not found", userId)
	}
	return user.Status, nil
}
