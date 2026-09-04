package model

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	InternalAccountErrorInvalidRequest   = "invalid_request"
	InternalAccountErrorUserNotFound     = "newapi_user_not_found"
	InternalAccountErrorStoreUnavailable = "store_unavailable"
	internalAccountUsernamePrefix        = "iap-"
	internalAccountProvisionKeyMaxLength = 64
)

var errInternalAccountProvisionRace = errors.New("internal account provision already exists")

type InternalAccountProvision struct {
	Id           int    `json:"id"`
	ProvisionKey string `json:"provision_key" gorm:"type:varchar(64);uniqueIndex;not null"`
	UserId       int    `json:"user_id" gorm:"column:user_id;index;not null"`
	CreatedTime  int64  `json:"created_time" gorm:"autoCreateTime;column:created_time"`
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

func ProvisionInternalAccount(provisionKey string) (int, *InternalAccountError) {
	validatedKey, validationErr := ValidateInternalProvisionKey(provisionKey)
	if validationErr != nil {
		return 0, validationErr
	}

	var userId int
	err := DB.Transaction(func(tx *gorm.DB) error {
		var existing InternalAccountProvision
		found := tx.Where("provision_key = ?", validatedKey).Limit(1).Find(&existing)
		if found.Error != nil {
			return found.Error
		}
		if found.RowsAffected > 0 {
			userId = existing.UserId
			return nil
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
		if query.Error == nil && query.RowsAffected > 0 {
			return existing.UserId, nil
		}
		if query.Error != nil {
			err = query.Error
		}
	}
	if err != nil {
		return 0, internalAccountStorageError(err)
	}
	return userId, nil
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
