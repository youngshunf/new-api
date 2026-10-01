package main

import (
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
	db, err := gorm.Open(postgres.Open("host=127.0.0.1 port=15433 user=tk_sotc12_s1a password=tk_sotc12_s1a_local dbname=tk_sotc12_s1a sslmode=disable"), &gorm.Config{})
	if err != nil {
		panic(err)
	}
	var database string
	if err := db.Raw("SELECT current_database()").Scan(&database).Error; err != nil || database != "tk_sotc12_s1a" {
		panic("生成目标必须是本任务隔离库")
	}
	out := filepath.Join("..", "..", "model")
	g := gen.NewGenerator(gen.Config{OutPath: filepath.Join(os.TempDir(), "tk-sotc12-modelgen-query"), ModelPkgPath: out, FieldNullable: true, FieldCoverable: false})
	g.UseDB(db)
	g.GenerateModelAs("audio_request_settlements", "AudioRequestSettlement",
		gen.FieldType("created_time", "DatabaseTime"),
		gen.FieldType("updated_time", "DatabaseTime"),
		gen.FieldType("completed_time", "*DatabaseTime"),
	)
	g.Execute()
	path := filepath.Join(out, "audio_request_settlements.gen.go")
	data, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	// 机械处理生成文本：中文注释与本仓ID命名，不手写或增删真实表字段。
	text := strings.ReplaceAll(string(data), "ID", "Id")
	text = regexp.MustCompile(`(?m)^//.*\n`).ReplaceAllString(text, "")
	text = strings.ReplaceAll(text, "type AudioRequestSettlement struct", "// AudioRequestSettlement 由正式SQL建表后的真实列生成，分类为platform_mutable_entity。\ntype AudioRequestSettlement struct")
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
