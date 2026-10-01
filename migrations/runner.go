package migrations

import (
	"crypto/sha256"
	"embed"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
)

// sqlFiles 只承载正式forward-only迁移，不从运行环境加载额外SQL。
//
//go:embed sqlite/*.sql postgres/*.sql mysql/*.sql
var sqlFiles embed.FS

var addColumnPattern = regexp.MustCompile(`^ALTER TABLE tokens ADD COLUMN ([a-z_]+) `)

// Apply 在旧baseline之后执行正式迁移；失败必须阻止启动，不标记部分DDL已完成。
func Apply(db *gorm.DB) error {
	return db.Connection(func(conn *gorm.DB) error {
		switch db.Dialector.Name() {
		case "mysql":
			var acquired int
			if err := conn.Raw("SELECT GET_LOCK('newapi_schema_migrations', 30)").Scan(&acquired).Error; err != nil || acquired != 1 {
				return fmt.Errorf("获取迁移锁失败：acquired=%d err=%v", acquired, err)
			}
			defer conn.Exec("SELECT RELEASE_LOCK('newapi_schema_migrations')")
			return apply(conn)
		case "postgres", "sqlite":
			return conn.Transaction(func(tx *gorm.DB) error {
				if db.Dialector.Name() == "postgres" {
					if err := tx.Exec("SELECT pg_advisory_xact_lock(73412001)").Error; err != nil {
						return err
					}
				}
				return apply(tx)
			})
		default:
			return fmt.Errorf("不支持迁移方言：%s", db.Dialector.Name())
		}
	})
}

func migrationFiles(db *gorm.DB) ([]string, error) {
	entries, err := sqlFiles.ReadDir(db.Dialector.Name())
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func migrationSource(db *gorm.DB, name string) (string, string, error) {
	data, err := sqlFiles.ReadFile(db.Dialector.Name() + "/" + name)
	if err != nil {
		return "", "", err
	}
	return string(data), fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

func statements(source string) []string {
	var lines []string
	for _, line := range strings.Split(source, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			lines = append(lines, line)
		}
	}
	var result []string
	for _, statement := range strings.Split(strings.Join(lines, "\n"), ";") {
		if value := strings.TrimSpace(statement); value != "" {
			result = append(result, value)
		}
	}
	return result
}

func apply(db *gorm.DB) error {
	db = db.Session(&gorm.Session{NewDB: true})
	names, err := migrationFiles(db)
	if err != nil {
		return err
	}
	for _, name := range names {
		source, checksum, err := migrationSource(db, name)
		if err != nil {
			return err
		}
		parts := statements(source)
		// 第一条只建立该schema owner的框架账本，不生成业务model。
		if err := db.Exec(parts[0]).Error; err != nil {
			return err
		}
		var existing []string
		if err := db.Table("schema_migrations").Where("migration_id = ?", name).Pluck("checksum", &existing).Error; err != nil {
			return err
		}
		if len(existing) > 0 {
			if existing[0] != checksum {
				return fmt.Errorf("已应用迁移摘要不匹配：%s", name)
			}
			continue
		}
		for _, statement := range parts[1:] {
			// MySQL DDL会隐式提交。恢复只能核实际已建列，不能假称整文件可回滚。
			if match := addColumnPattern.FindStringSubmatch(statement); len(match) == 2 && db.Migrator().HasColumn("tokens", match[1]) {
				continue
			}
			if strings.HasPrefix(statement, "CREATE UNIQUE INDEX idx_tokens_external_lease_id") && db.Migrator().HasIndex("tokens", "idx_tokens_external_lease_id") {
				continue
			}
			if strings.HasPrefix(statement, "CREATE UNIQUE INDEX idx_audio_settlement_lookup") && db.Migrator().HasIndex("audio_request_settlements", "idx_audio_settlement_lookup") {
				continue
			}
			if err := db.Exec(statement).Error; err != nil {
				return fmt.Errorf("迁移%s执行失败：%w", name, err)
			}
		}
		if err := verifyAudioSchema(db); err != nil {
			return err
		}
		created := any(time.Now().UTC())
		if db.Dialector.Name() == "sqlite" {
			created = time.Now().UnixMilli()
		}
		if err := db.Table("schema_migrations").Create(map[string]any{"migration_id": name, "checksum": checksum, "created_time": created}).Error; err != nil {
			return err
		}
	}
	return verifyAudioSchema(db)
}

// Verify 是非master启动门；只核版本、摘要和真实目标schema，不执行DDL。
func Verify(db *gorm.DB) error {
	db = db.Session(&gorm.Session{NewDB: true})
	names, err := migrationFiles(db)
	if err != nil {
		return err
	}
	if !db.Migrator().HasTable("schema_migrations") {
		return fmt.Errorf("缺少正式迁移账本")
	}
	for _, name := range names {
		_, checksum, err := migrationSource(db, name)
		if err != nil {
			return err
		}
		var values []string
		if err := db.Table("schema_migrations").Where("migration_id = ?", name).Pluck("checksum", &values).Error; err != nil {
			return err
		}
		if len(values) != 1 || values[0] != checksum {
			return fmt.Errorf("必需迁移未完成或摘要错误：%s", name)
		}
	}
	return verifyAudioSchema(db)
}

func verifyAudioSchema(db *gorm.DB) error {
	for _, table := range []string{"tokens", "audio_request_settlements"} {
		columns, err := db.Table(table).Migrator().ColumnTypes(table)
		if err != nil {
			return err
		}
		actual := make(map[string]gorm.ColumnType)
		for _, column := range columns {
			actual[column.Name()] = column
		}
		if table == "tokens" {
			for _, name := range []string{"purpose", "accounting_mode", "external_lease_id"} {
				column, ok := actual[name]
				if !ok {
					return fmt.Errorf("缺少目标列：%s.%s", table, name)
				}
				nullable, known := column.Nullable()
				if known && nullable != (name == "external_lease_id") {
					return fmt.Errorf("目标列可空性错误：%s.%s", table, name)
				}
				wantType := "varchar"
				if db.Dialector.Name() == "sqlite" {
					wantType = "text"
				}
				if strings.ToLower(column.DatabaseTypeName()) != wantType {
					return fmt.Errorf("目标令牌列类型错误：%s.%s", table, name)
				}
				if wantType == "varchar" {
					wantLength := int64(32)
					if name == "accounting_mode" {
						wantLength = 16
					}
					if name == "external_lease_id" {
						wantLength = 64
					}
					if length, known := column.Length(); known && length != wantLength {
						return fmt.Errorf("目标令牌列长度错误：%s.%s", table, name)
					}
				}
				if name != "external_lease_id" {
					wantDefault := "generic"
					if name == "accounting_mode" {
						wantDefault = "legacy"
					}
					if value, hasDefault := column.DefaultValue(); hasDefault && !strings.Contains(value, wantDefault) {
						return fmt.Errorf("目标令牌缺省值错误：%s.%s", table, name)
					}
				}
			}
			if err := verifyUniqueIndex(db, table, "idx_tokens_external_lease_id", []string{"external_lease_id"}); err != nil {
				return err
			}
			continue
		}
		source, _, err := migrationSource(db, "V001__音频同步账务底座.sql")
		if err != nil {
			return err
		}
		body := source[strings.Index(source, "CREATE TABLE IF NOT EXISTS audio_request_settlements (")+len("CREATE TABLE IF NOT EXISTS audio_request_settlements ("):]
		body = body[:strings.Index(body, "\n)")]
		expected := 0
		for _, line := range strings.Split(body, "\n") {
			fields := strings.Fields(strings.TrimSpace(line))
			if len(fields) < 2 || strings.HasPrefix(fields[0], "UNIQUE") {
				continue
			}
			expected++
			column, ok := actual[fields[0]]
			if !ok {
				return fmt.Errorf("缺少目标列：%s.%s", table, fields[0])
			}
			wantNullable := !strings.Contains(line, "NOT NULL")
			if nullable, known := column.Nullable(); known && nullable != wantNullable {
				return fmt.Errorf("目标列可空性错误：%s.%s", table, fields[0])
			}
			actualType := strings.ToLower(column.DatabaseTypeName())
			wantedType := strings.ToLower(strings.Split(fields[1], "(")[0])
			if db.Dialector.Name() == "postgres" && wantedType == "bigint" {
				wantedType = "int8"
			}
			if actualType != wantedType {
				return fmt.Errorf("目标列类型错误：%s.%s expected=%s actual=%s", table, fields[0], wantedType, actualType)
			}
			if wantedType == "varchar" || wantedType == "char" {
				if length, known := column.Length(); known && fmt.Sprintf("%s(%d)", wantedType, length) != fields[1] {
					return fmt.Errorf("目标列长度错误：%s.%s", table, fields[0])
				}
			}
		}
		if len(actual) != expected {
			return fmt.Errorf("目标列集合错误：%s expected=%d actual=%d", table, expected, len(actual))
		}
		if err := verifyUniqueIndex(db, table, "idx_audio_settlement_lookup", []string{"external_lease_id", "gateway_request_id"}); err != nil {
			return err
		}
	}
	return nil
}

// verifyUniqueIndex 核实际唯一性与完整列集合，不能凭同名普通索引冒充DB唯一绑定。
func verifyUniqueIndex(db *gorm.DB, table, name string, columns []string) error {
	if db.Dialector.Name() == "sqlite" {
		var rows []struct {
			Name   string
			Unique int
		}
		if err := db.Raw("PRAGMA index_list(" + table + ")").Scan(&rows).Error; err != nil {
			return err
		}
		found := false
		for _, row := range rows {
			if row.Name == name && row.Unique == 1 {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("缺少真正唯一索引：%s", name)
		}
		var fields []struct{ Name string }
		if err := db.Raw("PRAGMA index_info(" + name + ")").Scan(&fields).Error; err != nil {
			return err
		}
		if len(fields) != len(columns) {
			return fmt.Errorf("唯一索引列集合错误：%s", name)
		}
		for i, field := range fields {
			if field.Name != columns[i] {
				return fmt.Errorf("唯一索引列错误：%s", name)
			}
		}
		return nil
	}
	indexes, err := db.Migrator().GetIndexes(table)
	if err != nil {
		return err
	}
	for _, index := range indexes {
		if index.Name() != name {
			continue
		}
		unique, known := index.Unique()
		if !known || !unique || strings.Join(index.Columns(), ",") != strings.Join(columns, ",") {
			return fmt.Errorf("唯一索引实际形状错误：%s", name)
		}
		return nil
	}
	return fmt.Errorf("缺少真正唯一索引：%s", name)
}
