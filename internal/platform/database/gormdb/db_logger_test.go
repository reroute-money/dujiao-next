package gormdb

import (
	"bytes"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/migrator"
	"gorm.io/gorm/schema"
)

type gormLogRecorder struct {
	bytes.Buffer
}

func (recorder *gormLogRecorder) Printf(format string, args ...interface{}) {
	_, _ = fmt.Fprintf(&recorder.Buffer, format, args...)
}

func TestReleaseGORMLoggerDoesNotExposeQueryParameters(t *testing.T) {
	recorder := &gormLogRecorder{}
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: newGORMLogger(" release ", recorder),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}

	secret := "must-not-appear-in-production-database-logs"
	if err := db.Exec("INSERT INTO missing_table (secret) VALUES (?)", secret).Error; err == nil {
		t.Fatal("expected missing-table error")
	}

	output := recorder.String()
	if output == "" {
		t.Fatal("expected database error to be logged")
	}
	if strings.Contains(output, secret) {
		t.Fatalf("release database log exposed a query parameter: %s", output)
	}
	if !strings.Contains(output, "missing_table") {
		t.Fatalf("release database log should retain useful query context: %s", output)
	}
}

// uniqueMigrateRecorder 记录基类是否被调用，用来断言 cockroachMigrator 的分支取舍。
type uniqueMigrateRecorder struct {
	gorm.Migrator
	calls int
}

// crdbUniqueFixture 对照项目里两种唯一列写法在 schema 解析后的差异。
type crdbUniqueFixture struct {
	ID       uint   `gorm:"primarykey"`
	Username string `gorm:"uniqueIndex;not null"`
	Email    string `gorm:"unique;not null"`
}

func (recorder *uniqueMigrateRecorder) MigrateColumnUnique(dst interface{}, field *schema.Field, columnType gorm.ColumnType) error {
	recorder.calls++
	return nil
}

func TestCockroachMigratorKeepsOnlyUniqueConstraintCreation(t *testing.T) {
	recorder := &uniqueMigrateRecorder{}
	migratorDB := cockroachMigrator{Migrator: recorder}
	// CRDB 把 uniqueIndex 建的索引上报成表级约束，于是 columnType.Unique()=true
	uniqueColumnType := migrator.ColumnType{UniqueValue: sql.NullBool{Bool: true, Valid: true}}

	// 拦不拦得住取决于 field.Unique，而项目里的唯一列一律标 uniqueIndex
	sqliteDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{DryRun: true, DisableAutomaticPing: true})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	stmt := &gorm.Statement{DB: sqliteDB}
	if err := stmt.Parse(&crdbUniqueFixture{}); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	username := stmt.Schema.FieldsByDBName["username"]
	if username.Unique {
		t.Fatal("uniqueIndex must not set Unique")
	}
	// 唯一性来自 ParseIndexes 的 UNIQUE 索引，AutoMigrate 走 CreateIndex 而不是约束
	var indexed bool
	for _, idx := range stmt.Schema.ParseIndexes() {
		if idx.Class == "UNIQUE" && len(idx.Fields) == 1 && idx.Fields[0].Field.DBName == "username" {
			indexed = true
		}
	}
	if !indexed {
		t.Fatal("uniqueIndex column must be covered by a UNIQUE index")
	}
	if err := migratorDB.MigrateColumnUnique(&crdbUniqueFixture{}, username, uniqueColumnType); err != nil {
		t.Fatalf("migrate uniqueIndex column: %v", err)
	}
	if recorder.calls != 0 {
		t.Fatal("uniqueIndex column must not reach unique constraint migration")
	}

	email := stmt.Schema.FieldsByDBName["email"]
	if !email.Unique {
		t.Fatal(`gorm:"unique" must set Unique`)
	}
	if err := migratorDB.MigrateColumnUnique(&crdbUniqueFixture{}, email,
		migrator.ColumnType{UniqueValue: sql.NullBool{Valid: true}}); err != nil {
		t.Fatalf(`migrate gorm:"unique" column: %v`, err)
	}
	if recorder.calls != 1 {
		t.Fatalf(`gorm:"unique" column must still get its constraint created, calls=%d`, recorder.calls)
	}
}

func TestNewDialectorForCockroachKeepsPostgresDialect(t *testing.T) {
	cockroachDSN := "host=127.0.0.1 port=1 user=dujiao dbname=dujiao sslmode=disable"
	dialector, err := newDialector("CockroachDB", cockroachDSN)
	if err != nil {
		t.Fatalf("newDialector: %v", err)
	}
	if _, ok := dialector.(cockroachDialector); !ok {
		t.Fatalf("cockroach driver dialector = %T, want cockroachDialector", dialector)
	}
	// 按方言分支的裸 SQL（gormutil 等）依赖 Name() 仍然是 postgres
	if got := dialector.Name(); got != "postgres" {
		t.Fatalf("cockroach dialector name = %q, want postgres", got)
	}

	db, err := gorm.Open(dialector, &gorm.Config{DryRun: true, DisableAutomaticPing: true})
	if err != nil {
		t.Fatalf("open cockroach db: %v", err)
	}
	if _, ok := db.Migrator().(cockroachMigrator); !ok {
		t.Fatalf("migrator = %T, want cockroachMigrator", db.Migrator())
	}

	plain, err := newDialector("postgres", cockroachDSN)
	if err != nil {
		t.Fatalf("newDialector postgres: %v", err)
	}
	if _, ok := plain.(cockroachDialector); ok {
		t.Fatal("postgres driver must not use the cockroach shim")
	}

	if _, err := newDialector("mysql", "dsn"); err == nil {
		t.Fatal("unsupported database driver must fail")
	}
}
