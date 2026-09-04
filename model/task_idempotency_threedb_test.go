package model

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"

	"github.com/glebarez/sqlite"
	mysqldriver "gorm.io/driver/mysql"
	pgdriver "gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// 三库兼容验证。用真实的 SQLite / MySQL / PostgreSQL 实例跑，不 mock。
// 通过 NEWAPI_THREEDB_VERIFY=1 打开。
func TestThreeDatabaseCompatibility(t *testing.T) {
	if os.Getenv("NEWAPI_THREEDB_VERIFY") != "1" {
		t.Skip("set NEWAPI_THREEDB_VERIFY=1 to run")
	}
	originalDB := DB
	defer func() { DB = originalDB }()

	dialects := []struct {
		name    string
		dbType  common.DatabaseType
		open    func() (*gorm.DB, error)
		version string
	}{
		{"sqlite", common.DatabaseTypeSQLite, func() (*gorm.DB, error) {
			return gorm.Open(sqlite.Open(t.TempDir()+"/verify.db"), &gorm.Config{})
		}, "SELECT sqlite_version()"},
		{"mysql", common.DatabaseTypeMySQL, func() (*gorm.DB, error) {
			return gorm.Open(mysqldriver.Open(os.Getenv("VERIFY_MYSQL_DSN")), &gorm.Config{})
		}, "SELECT version()"},
		{"postgres", common.DatabaseTypePostgreSQL, func() (*gorm.DB, error) {
			return gorm.Open(pgdriver.Open(os.Getenv("VERIFY_PG_DSN")), &gorm.Config{})
		}, "SELECT version()"},
	}

	for _, d := range dialects {
		t.Run(d.name, func(t *testing.T) {
			db, err := d.open()
			if err != nil {
				t.Fatalf("open %s: %v", d.name, err)
			}
			DB = db
			common.SetDatabaseTypes(d.dbType, d.dbType)

			var version string
			if err := db.Raw(d.version).Scan(&version).Error; err != nil {
				t.Fatalf("version %s: %v", d.name, err)
			}
			t.Logf("[%s] server version = %s", d.name, strings.Split(version, "\n")[0])

			// ① 升级路径：先建出「改动之前」那套代表性数据库（只有 tasks），并放一行真实数据。
			if err := db.Migrator().DropTable(&TaskIdempotencyRecord{}, &Task{}); err != nil {
				t.Fatalf("drop: %v", err)
			}
			if err := db.AutoMigrate(&Task{}); err != nil {
				t.Fatalf("baseline migrate: %v", err)
			}
			legacy := &Task{TaskID: "legacy_task_1", UserId: 4242, Platform: "verify", Status: TaskStatusSuccess,
				Properties: Properties{OriginModelName: "sora-2"}}
			if err := db.Create(legacy).Error; err != nil {
				t.Fatalf("seed legacy task: %v", err)
			}

			// ② 迁移跑两遍，证明幂等（第二遍不得报错、不得改变结构）。
			for round := 1; round <= 2; round++ {
				if err := db.AutoMigrate(&Task{}, &TaskIdempotencyRecord{}); err != nil {
					t.Fatalf("migrate round %d: %v", round, err)
				}
			}
			if !db.Migrator().HasTable(&TaskIdempotencyRecord{}) {
				t.Fatal("task_idempotency_records missing after migrate")
			}
			if !db.Migrator().HasIndex(&TaskIdempotencyRecord{}, "ScopeHash") {
				t.Fatal("unique index on scope_hash missing")
			}

			// ③ 存量数据与索引在升级后仍在。
			var survivor Task
			if err := db.Where("task_id = ?", "legacy_task_1").First(&survivor).Error; err != nil {
				t.Fatalf("legacy task lost: %v", err)
			}
			if survivor.Properties.OriginModelName != "sora-2" {
				t.Fatalf("legacy task data corrupted: %+v", survivor.Properties)
			}

			// ④ 唯一约束在这一库上真的生效：直接插两行同 scope_hash 必须失败。
			db.Where("1 = 1").Delete(&TaskIdempotencyRecord{})
			dup := func() error {
				return db.Create(&TaskIdempotencyRecord{ScopeHash: "dup-hash", TokenId: 1,
					Operation: "POST /v1/video/generations", IdempotencyKey: "k", RequestDigest: "d",
					Status: TaskIdempotencyStatusProcessing}).Error
			}
			if err := dup(); err != nil {
				t.Fatalf("first insert: %v", err)
			}
			dupErr := dup()
			if dupErr == nil {
				t.Fatal("unique index did not reject the duplicate scope_hash")
			}
			if !isTaskIdempotencyDuplicate(dupErr) {
				t.Fatalf("duplicate error not recognised on %s: %v", d.name, dupErr)
			}
			t.Logf("[%s] duplicate error recognised: %v", d.name, dupErr)

			// ⑤ 完整幂等语义在这一库上跑一遍。
			db.Where("1 = 1").Delete(&TaskIdempotencyRecord{})
			scope := TaskIdempotencyScope{TokenId: 77, Operation: "POST /v1/video/generations",
				IdempotencyKey: "verify-key", RequestDigest: "digest-a"}

			record, replay, acqErr := AcquireTaskIdempotency(scope)
			if acqErr != nil || record == nil || replay != nil {
				t.Fatalf("first acquire: record=%v replay=%v err=%v", record, replay, acqErr)
			}
			if _, _, inflight := AcquireTaskIdempotency(scope); inflight == nil || inflight.Code != TaskIdempotencyErrorInProgress {
				t.Fatalf("in-flight resend must be rejected, got %v", inflight)
			}
			if err := CompleteTaskIdempotency(record, "task_verify_1"); err != nil {
				t.Fatalf("complete: %v", err)
			}
			_, replay, acqErr = AcquireTaskIdempotency(scope)
			if acqErr != nil || replay == nil || replay.TaskID != "task_verify_1" {
				t.Fatalf("replay: replay=%v err=%v", replay, acqErr)
			}
			conflictScope := scope
			conflictScope.RequestDigest = "digest-b"
			_, _, conflictErr := AcquireTaskIdempotency(conflictScope)
			if conflictErr == nil || conflictErr.Code != TaskIdempotencyErrorConflict || conflictErr.HTTPStatus != http.StatusConflict {
				t.Fatalf("conflict: %v", conflictErr)
			}

			// ⑥ 并发只能有一个占到键——这是唯一索引在该库上的真实互斥验证。
			db.Where("1 = 1").Delete(&TaskIdempotencyRecord{})
			raceScope := scope
			raceScope.IdempotencyKey = "verify-race"
			var mu sync.Mutex
			won := 0
			var wg sync.WaitGroup
			for i := 0; i < 6; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					r, _, _ := AcquireTaskIdempotency(raceScope)
					if r != nil {
						mu.Lock()
						won++
						mu.Unlock()
					}
				}()
			}
			wg.Wait()
			if won != 1 {
				t.Fatalf("concurrent acquire winners = %d, want 1", won)
			}

			// 收尾：把验证库清干净。
			if err := db.Migrator().DropTable(&TaskIdempotencyRecord{}, &Task{}); err != nil {
				t.Fatalf("cleanup: %v", err)
			}
			fmt.Printf("[%s] all checks passed\n", d.name)
		})
	}
}
