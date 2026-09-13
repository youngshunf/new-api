package model

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/dto"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// 中文审核理由按数据库字符上限截断，不能截断一个 UTF-8 编码。
func TestCreditReasonPreservesUtf8AtCharacterLimit(t *testing.T) {
	for _, reason := range []string{strings.Repeat("验", 70), strings.Repeat("x", 63) + "🌟结束", "文本链路验收"} {
		req := &dto.CreditOperationRequest{OperationType: CreditOperationWalletGrant, NewApiUserId: 1, CreditAmount: "0.02", Reason: reason}
		normalized, err := normalizeCreditOperationRequest(req)
		require.Nil(t, err)
		expected := []rune(reason)
		if len(expected) > 64 {
			expected = expected[:64]
		}
		assert.True(t, utf8.ValidString(normalized.reason))
		assert.Equal(t, string(expected), normalized.reason)
	}
}

// 三种真实数据库都必须能保存经生产入口校验后的多字节理由。
func TestCreditReasonPersistsAcrossDatabases(t *testing.T) {
	cases := []struct {
		name string
		open func() gorm.Dialector
	}{
		{"sqlite", func() gorm.Dialector { return sqlite.Open(t.TempDir() + "/credit.db") }},
		{"mysql", func() gorm.Dialector { return mysql.Open(os.Getenv("TEST_MYSQL_DSN")) }},
		{"postgres", func() gorm.Dialector { return postgres.Open(os.Getenv("TEST_POSTGRES_DSN")) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.name != "sqlite" && os.Getenv("TEST_"+strings.ToUpper(c.name)+"_DSN") == "" {
				t.Skip("缺真实数据库配置")
			}
			db, err := gorm.Open(c.open(), &gorm.Config{})
			require.NoError(t, err)
			connection, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, connection.Close()) })
			table := fmt.Sprintf("credit_reason_it_%d", time.Now().UnixNano())
			t.Cleanup(func() { require.NoError(t, db.Migrator().DropTable(table)) })
			store := db.Table(table)
			require.NoError(t, store.AutoMigrate(&CreditOperation{}))
			req := &dto.CreditOperationRequest{OperationType: CreditOperationWalletGrant, NewApiUserId: 1, CreditAmount: "0.02", Reason: strings.Repeat("验", 63) + "🌟结束"}
			normalized, rejected := normalizeCreditOperationRequest(req)
			require.Nil(t, rejected)
			row := CreditOperation{EventId: "utf8-reason", Reason: normalized.reason}
			require.NoError(t, store.Create(&row).Error)
			var saved CreditOperation
			require.NoError(t, db.Table(table).First(&saved, row.Id).Error)
			assert.Equal(t, strings.Repeat("验", 63)+"🌟", saved.Reason)
		})
	}
}
