package model

import (
	"database/sql/driver"
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
)

// DatabaseTime 仅适配正式公共时间列：PG/MySQL使用UTC时间，SQLite持久Unix毫秒。
type DatabaseTime struct{ time.Time }

func (t DatabaseTime) Value() (driver.Value, error) {
	if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
		return t.UTC().UnixMilli(), nil
	}
	return t.UTC().Truncate(time.Millisecond), nil
}

func (t *DatabaseTime) Scan(value any) error {
	switch v := value.(type) {
	case time.Time:
		t.Time = v.UTC()
	case int64:
		t.Time = time.UnixMilli(v).UTC()
	case []byte:
		return t.Scan(string(v))
	case string:
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999"} {
			if parsed, err := time.Parse(layout, v); err == nil {
				t.Time = parsed.UTC()
				return nil
			}
		}
		return fmt.Errorf("无法解析数据库公共时间")
	default:
		return fmt.Errorf("不支持的数据库公共时间类型：%T", value)
	}
	return nil
}
