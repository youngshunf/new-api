package model

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// 每种引擎使用独立表名；只回收本次测试的表，绝不接触运行中的库存状态。
func TestLlmInventoryPersistentRevision(t *testing.T) {
	cases := []struct {
		name string
		kind common.DatabaseType
		open func() gorm.Dialector
	}{
		{"sqlite", common.DatabaseTypeSQLite, func() gorm.Dialector {
			return sqlite.Open(filepath.Join(t.TempDir(), "inventory.db"))
		}},
		{"mysql", common.DatabaseTypeMySQL, func() gorm.Dialector {
			return mysql.Open(os.Getenv("TEST_MYSQL_DSN"))
		}},
		{"postgres", common.DatabaseTypePostgreSQL, func() gorm.Dialector {
			return postgres.Open(os.Getenv("TEST_POSTGRES_DSN"))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.name != "sqlite" && os.Getenv("TEST_"+strings.ToUpper(c.name)+"_DSN") == "" {
				t.Skip("未提供真实数据库 DSN")
			}
			oldMain, oldLog := common.MainDatabaseType(), common.LogDatabaseType()
			common.SetDatabaseTypes(c.kind, c.kind)
			t.Cleanup(func() { common.SetDatabaseTypes(oldMain, oldLog) })
			db, err := gorm.Open(c.open(), &gorm.Config{})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
			if c.name == "sqlite" {
				sqlDB.SetMaxOpenConns(1)
			}
			table := fmt.Sprintf("llm_inventory_it_%d", time.Now().UnixNano())
			t.Cleanup(func() { require.NoError(t, db.Migrator().DropTable(table)) })
			store := db.Table(table)
			// 从无此表的旧版数据库扩展，不修改任何旧表；连续迁移不改变已存版本。
			require.NoError(t, store.AutoMigrate(&LlmInventoryState{}))
			digestA, digestB := strings.Repeat("a", 64), strings.Repeat("b", 64)
			first, err := ObserveLlmInventory(store, digestA)
			require.NoError(t, err)
			assert.EqualValues(t, 1, first)
			for range 2 {
				require.NoError(t, store.AutoMigrate(&LlmInventoryState{}))
			}
			repeated, err := ObserveLlmInventory(db.Session(&gorm.Session{NewDB: true}).Table(table), digestA)
			require.NoError(t, err)
			assert.Equal(t, first, repeated)
			// 两个真实事务同时观察相同变更，只能分配同一版本。
			var wg sync.WaitGroup
			results, errs := make([]int64, 2), make([]error, 2)
			for i := range 2 {
				wg.Add(1)
				go func() { defer wg.Done(); results[i], errs[i] = ObserveLlmInventory(db.Table(table), digestB) }()
			}
			wg.Wait()
			for i := range 2 {
				require.NoError(t, errs[i])
				assert.EqualValues(t, 2, results[i])
			}
			returned, err := ObserveLlmInventory(store, digestA)
			require.NoError(t, err)
			assert.EqualValues(t, 3, returned, "内容回退仍是新版本，不能倒退到第一次的版本")
			_, err = ObserveLlmInventory(store, "invalid")
			require.Error(t, err)
			var rows int64
			require.NoError(t, store.Count(&rows).Error)
			assert.EqualValues(t, 1, rows, "并发不得生成第二个 current 状态")
		})
	}
}
