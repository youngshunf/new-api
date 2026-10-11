package main

import (
	"flag"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gorm.io/driver/postgres"
	"gorm.io/gen"
	"gorm.io/gorm"
)

func main() {
	// 只读取本任务runner创建的真实隔离PG表；不读取共享业务库或敏感DSN文件。
	table := flag.String("table", "audio_request_settlements", "正式迁移已创建的目标表")
	flag.Parse()
	database, structName := "tk_sotc12_s1a", "AudioRequestSettlement"
	dsn := "host=127.0.0.1 port=15433 user=tk_sotc12_s1a password=tk_sotc12_s1a_local dbname=tk_sotc12_s1a sslmode=disable"
	timeFields := []gen.ModelOpt{gen.FieldType("created_time", "DatabaseTime"), gen.FieldType("updated_time", "DatabaseTime"), gen.FieldType("completed_time", "*DatabaseTime")}
	if *table == "voice_profiles" {
		database, structName = "vc7_newapi_voice", "VoiceProfile"
		dsn = "host=127.0.0.1 port=15433 user=hasn_dev password=hasn_dev dbname=vc7_newapi_voice sslmode=disable"
		timeFields = []gen.ModelOpt{gen.FieldType("created_time", "DatabaseTime"), gen.FieldType("updated_time", "DatabaseTime"),
			gen.FieldType("delete_requested_time", "*DatabaseTime"), gen.FieldType("delete_dispatched_time", "*DatabaseTime"), gen.FieldType("deleted_time", "*DatabaseTime")}
	} else if *table != "audio_request_settlements" {
		panic("未登记的生成表")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		panic(err)
	}
	var actualDatabase string
	if err := db.Raw("SELECT current_database()").Scan(&actualDatabase).Error; err != nil || actualDatabase != database {
		panic("生成目标必须是本任务隔离库")
	}
	out := filepath.Join("..", "..", "model")
	g := gen.NewGenerator(gen.Config{OutPath: filepath.Join(os.TempDir(), "hasn-modelgen-"+*table), ModelPkgPath: out, FieldNullable: true, FieldCoverable: false})
	g.UseDB(db)
	g.GenerateModelAs(*table, structName, timeFields...)
	g.Execute()
	path := filepath.Join(out, *table+".gen.go")
	data, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	// 机械处理生成文本：中文注释与本仓ID命名，不手写或增删真实表字段。
	text := strings.ReplaceAll(string(data), "ID", "Id")
	text = regexp.MustCompile(`(?m)^//.*\n`).ReplaceAllString(text, "")
	text = strings.ReplaceAll(text, "type "+structName+" struct", "// "+structName+" 由正式SQL建表后的真实列生成，分类为platform_mutable_entity。\ntype "+structName+" struct")
	text = "// 本文件由tools/modelgen读取真实数据库生成，禁止手写新表字段。\n" + text
	data, err = format.Source([]byte(text))
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		panic(err)
	}
	fmt.Println("已从真实schema生成单表model：", path)
}
