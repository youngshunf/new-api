package modeltests

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/migrations"
	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// VerifyManagedAudioCacheAndFailure 由同包入口保证真实列名已初始化，只用共享Redis的唯一任务键。
func VerifyManagedAudioCacheAndFailure(t *testing.T) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/cache.db"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Token{}))
	require.NoError(t, migrations.Apply(db))
	previousDB, previousRedis, previousBatch, previousRDB := model.DB, common.RedisEnabled, common.BatchUpdateEnabled, common.RDB
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6380", DB: 14})
	require.NoError(t, client.Ping(context.Background()).Err(), "必须连接真实Redis，不用miniredis替代新反例")
	model.DB, common.RedisEnabled, common.BatchUpdateEnabled, common.RDB = db, true, true, client
	key := fmt.Sprintf("tk_sotc12_audio_%d", time.Now().UnixNano())
	cacheKey := "token:" + common.GenerateHMAC(key)
	genericKey := key + "generic"
	genericCacheKey := "token:" + common.GenerateHMAC(genericKey)
	t.Cleanup(func() {
		_ = client.Del(context.Background(), cacheKey, "token:fence:"+common.GenerateHMAC(key), genericCacheKey).Err()
		_ = client.Close()
		model.DB, common.RedisEnabled, common.BatchUpdateEnabled, common.RDB = previousDB, previousRedis, previousBatch, previousRDB
	})
	require.NoError(t, db.Table("tokens").Create(map[string]any{"id": 140001, "user_id": 140002, "key": key, "status": common.TokenStatusEnabled, "expired_time": common.GetTimestamp() + 600, "remain_quota": 100, "purpose": "audio.tts", "accounting_mode": "synchronous", "external_lease_id": "tk.audio.cache"}).Error)
	token, err := model.GetTokenByKey(key, true)
	require.NoError(t, err)
	require.Equal(t, "synchronous", token.AccountingMode)
	mode, err := client.HGet(context.Background(), cacheKey, "AccountingMode").Result()
	require.NoError(t, err)
	assert.Equal(t, "synchronous", mode)
	require.NoError(t, client.HDel(context.Background(), cacheKey, "AccountingMode").Err())
	_, err = model.ValidateUserToken(key)
	assert.ErrorIs(t, err, model.ErrAudioAccountingUnavailable, "缺模式热缓存必须回DB，不得降格generic")
	require.NoError(t, client.HSet(context.Background(), cacheKey, "AccountingMode", "synchronous").Err())
	_, err = model.ValidateUserToken(key)
	assert.ErrorIs(t, err, model.ErrAudioAccountingUnavailable)

	// 旧generic仍以真实Redis额度原子预扣；新同步token并发拒绝不能破坏其batch策略。
	require.NoError(t, db.Table("tokens").Create(map[string]any{"id": 140010, "user_id": 140002, "key": genericKey, "status": common.TokenStatusEnabled, "expired_time": common.GetTimestamp() + 600, "remain_quota": 100}).Error)
	_, err = model.GetTokenByKey(genericKey, true)
	require.NoError(t, err)
	var successful atomic.Int32
	var refused atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := model.TryReserveTokenQuota(140010, genericKey, 25, false)
			if err == nil && ok {
				successful.Add(1)
			}
			_, err = model.TryReserveTokenQuota(140001, key, 25, false)
			if err == model.ErrAudioAccountingUnavailable {
				refused.Add(1)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(4), successful.Load())
	assert.Equal(t, int32(6), refused.Load())
	var genericRemaining int
	require.NoError(t, db.Table("tokens").Where("id = ?", 140010).Pluck("remain_quota", &genericRemaining).Error)
	assert.Equal(t, 100, genericRemaining, "旧generic batch尚未flush，不被本片全仓关闭")
	remainingCached, err := client.HGet(context.Background(), genericCacheKey, "RemainQuota").Int()
	require.NoError(t, err)
	assert.Equal(t, 0, remainingCached)

	// 真正关闭连接客户端验证Redis失效，不造service fallback；DB模式门仍不得写额度。
	require.NoError(t, client.Close())
	reserved, err := model.TryReserveTokenQuota(140001, key, 10, false)
	assert.False(t, reserved)
	assert.ErrorIs(t, err, model.ErrAudioAccountingUnavailable)
	assert.ErrorIs(t, model.IncreaseTokenQuota(140001, key, 10), model.ErrAudioAccountingUnavailable)
	var remaining int
	require.NoError(t, db.Table("tokens").Where("id = ?", 140001).Pluck("remain_quota", &remaining).Error)
	assert.Equal(t, 100, remaining)

	// 恢复真实Redis连接、保留旧热Key，再真实轮换DBKey，缓存不得继续授权旧Key。
	client = redis.NewClient(&redis.Options{Addr: "127.0.0.1:6380", DB: 14})
	common.RDB = client
	require.NoError(t, db.Table("tokens").Where("id = ?", 140001).Update("key", key+"new").Error)
	_, err = model.GetTokenByKey(key, false)
	assert.Error(t, err)
	require.NoError(t, db.Table("tokens").Where("id = ?", 140001).Update("key", key).Error)
	require.NoError(t, db.Table("tokens").Where("id = ?", 140001).Update("deleted_at", time.Now()).Error)
	_, err = model.GetTokenByKey(key, false)
	assert.Error(t, err, "软删受管token也不能沿热metadata授权")
	// 热metadata存在但真实DB断连时必须failclosed，不能把旧缓存当当前资格。
	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())
	_, err = model.GetTokenByKey(key, false)
	assert.Error(t, err)
}
