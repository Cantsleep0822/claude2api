package repository

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	turso "turso.tech/database/tursogo-serverless"
)

var db *gorm.DB

// InitDB 初始化 Turso 数据库。
func InitDB() error {
	databaseURL := strings.TrimSpace(os.Getenv("TURSO_DATABASE_URL"))
	authToken := strings.TrimSpace(os.Getenv("TURSO_AUTH_TOKEN"))
	if databaseURL == "" || authToken == "" {
		return fmt.Errorf("缺少 TURSO_DATABASE_URL 或 TURSO_AUTH_TOKEN")
	}

	sqlDB := sql.OpenDB(turso.NewConnector(databaseURL, authToken))
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return fmt.Errorf("连接 Turso 失败: %w", err)
	}

	conn, err := gorm.Open(
		&sqlite.Dialector{Conn: sqlDB},
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)},
	)
	if err != nil {
		_ = sqlDB.Close()
		return fmt.Errorf("打开数据库失败: %w", err)
	}

	if err := conn.AutoMigrate(&Account{}, &APIKey{}, &APILog{}); err != nil {
		_ = sqlDB.Close()
		return fmt.Errorf("建表失败: %w", err)
	}

	db = conn
	return nil
}

// nowUTC 返回 UTC 时间戳。
func nowUTC() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z")
}
