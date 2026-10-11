package gormdb

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/glebarez/sqlite" // 纯 Go SQLite 驱动（基于 modernc.org/sqlite）
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

var DB *gorm.DB

// DBPoolConfig 数据库连接池配置
type DBPoolConfig struct {
	MaxOpenConns           int
	MaxIdleConns           int
	ConnMaxLifetimeSeconds int
	ConnMaxIdleTimeSeconds int
}

// InitDB 初始化数据库连接
func InitDB(driver, dsn string, pool DBPoolConfig, mode string) error {
	normalized := strings.ToLower(strings.TrimSpace(driver))
	dialector, err := newDialector(driver, dsn)
	if err != nil {
		return err
	}
	DB, err = gorm.Open(dialector, &gorm.Config{
		Logger:  newGORMLogger(mode, nil),
		NowFunc: func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		return err
	}

	sqlDB, err := DB.DB()
	if err != nil {
		return err
	}
	applyDBPool(sqlDB, pool)

	// SQLite 额外执行 PRAGMA 确保 WAL 模式生效
	if normalized == "" || normalized == "sqlite" {
		DB.Exec("PRAGMA journal_mode=WAL")
		DB.Exec("PRAGMA busy_timeout=5000")
		DB.Exec("PRAGMA synchronous=NORMAL")
	}
	return nil
}

// newDialector 按 database.driver 构造 Dialector
func newDialector(driver, dsn string) (gorm.Dialector, error) {
	switch strings.ToLower(strings.TrimSpace(driver)) {
	case "", "sqlite":
		// glebarez/sqlite 是基于 modernc.org/sqlite 的纯 Go 驱动
		// 追加 PRAGMA 参数避免并发写入时 busy-spin 导致 CPU 飙升
		return sqlite.Open(appendSQLitePragmas(dsn)), nil
	case "postgres", "postgresql":
		return postgres.Open(dsn), nil
	case "cockroach", "cockroachdb":
		return cockroachDialector{Dialector: postgres.Open(dsn)}, nil
	default:
		return nil, fmt.Errorf("unsupported database driver: %s", driver)
	}
}

// cockroachDialector 让 CockroachDB 复用 postgres 驱动，只替换 Migrator。
// Name() 仍返回 postgres，方言分支（gormutil 等）不受影响。
type cockroachDialector struct {
	gorm.Dialector
}

func (dialector cockroachDialector) Migrator(db *gorm.DB) gorm.Migrator {
	return cockroachMigrator{Migrator: dialector.Dialector.Migrator(db)}
}

// cockroachMigrator 针对 CRDB 的类型/约束记账方式做两处适配：删唯一约束（见 MigrateColumnUnique）
// 和 JSON 别名（见 GetTypeAliases），其余迁移行为沿用 postgres 驱动。
// CRDB 把 unique index 也登记成表级约束，postgres 驱动据此对每个单列 uniqueIndex 发出
// DROP CONSTRAINT uni_<table>_<column>，而 CRDB 里约束名是 <table>_<column>_key，
// SQLSTATE 42704 会中断迁移。唯一性本就由索引保证，只保留创建分支。
type cockroachMigrator struct {
	gorm.Migrator
}

func (migrator cockroachMigrator) MigrateColumnUnique(dst interface{}, field *schema.Field, columnType gorm.ColumnType) error {
	if !field.Unique {
		return nil
	}
	return migrator.Migrator.MigrateColumnUnique(dst, field, columnType)
}

// GetTypeAliases 补上 CRDB 的 JSON 别名：CRDB 里 JSON 是 JSONB 的别名，列声明成 json 之后
// introspection 一律回报 jsonb，而模型的标签是 type:json。postgres 驱动的别名表没有这一对，
// AutoMigrate 每次比对都判定类型不符，对全部 JSON 列重发 ALTER COLUMN ... TYPE json USING ...::json，
// 每列触发一次全表重写（线上实测 2~3s/列，且永远不会收敛）。JSONB 与 JSON 在 CRDB 是同一物理类型，
// 这里把两个名字互认，让迁移比对收敛。
func (migrator cockroachMigrator) GetTypeAliases(databaseTypeName string) []string {
	switch strings.ToLower(databaseTypeName) {
	case "json", "jsonb":
		return []string{"json", "jsonb"}
	default:
		return migrator.Migrator.GetTypeAliases(databaseTypeName)
	}
}

func newGORMLogger(mode string, writer gormlogger.Writer) gormlogger.Interface {
	if !strings.EqualFold(strings.TrimSpace(mode), "release") {
		return gormlogger.Default.LogMode(gormlogger.Info)
	}
	if writer == nil {
		writer = log.New(os.Stdout, "\r\n", log.LstdFlags)
	}
	return gormlogger.New(writer, gormlogger.Config{
		SlowThreshold:             2 * time.Second,
		LogLevel:                  gormlogger.Warn,
		IgnoreRecordNotFoundError: true,
		ParameterizedQueries:      true,
		Colorful:                  false,
	})
}

// appendSQLitePragmas 在 SQLite DSN 中追加关键 PRAGMA 参数
func appendSQLitePragmas(dsn string) string {
	// glebarez/sqlite 支持 ?_pragma=key=value 形式
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep +
		"_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)"
}

func applyDBPool(sqlDB *sql.DB, pool DBPoolConfig) {
	if sqlDB == nil {
		return
	}
	if pool.MaxOpenConns > 0 {
		sqlDB.SetMaxOpenConns(pool.MaxOpenConns)
	}
	if pool.MaxIdleConns >= 0 {
		sqlDB.SetMaxIdleConns(pool.MaxIdleConns)
	}
	if pool.ConnMaxLifetimeSeconds > 0 {
		sqlDB.SetConnMaxLifetime(time.Duration(pool.ConnMaxLifetimeSeconds) * time.Second)
	}
	if pool.ConnMaxIdleTimeSeconds > 0 {
		sqlDB.SetConnMaxIdleTime(time.Duration(pool.ConnMaxIdleTimeSeconds) * time.Second)
	}
}
