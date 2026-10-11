package gormdb

import (
	"bytes"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/migrator"
	"gorm.io/gorm/schema"

	"github.com/dujiao-next/internal/shared/jsonmap"
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

// crdbJSONFixture 复刻项目里 JSON 快照列的声明方式（detail_json / title_json 等都是 type:json）。
type crdbJSONFixture struct {
	ID         uint         `gorm:"primarykey"`
	DetailJSON jsonmap.JSON `gorm:"column:detail_json;type:json"`
}

// alterColumnRecorder 记录列比对的结果：基类判定需要重写时才会走到 AlterColumn。
type alterColumnRecorder struct {
	gorm.Migrator
	altered []string
}

func (recorder *alterColumnRecorder) AlterColumn(dst interface{}, field string) error {
	recorder.altered = append(recorder.altered, field)
	return nil
}

// crdbMigratorStub 把 db.Migrator() 指向指定 Migrator。基类 MigrateColumn 的类型比对与
// AlterColumn 都通过 m.DB.Migrator() 解析，生产上那里拿到的正是 cockroachMigrator。
type crdbMigratorStub struct {
	gorm.Dialector
	migrator gorm.Migrator
}

func (stub crdbMigratorStub) Migrator(db *gorm.DB) gorm.Migrator { return stub.migrator }

// crdbColumnType 模拟 CRDB introspection 回报的 JSON 快照列：可空、无默认值、无注释、非唯一。
type crdbColumnType struct {
	name             string
	databaseTypeName string
}

func (ct crdbColumnType) Name() string { return ct.name }

func (ct crdbColumnType) DatabaseTypeName() string { return ct.databaseTypeName }

func (ct crdbColumnType) ColumnType() (string, bool) { return ct.databaseTypeName, true }

func (ct crdbColumnType) PrimaryKey() (bool, bool) { return false, false }

func (ct crdbColumnType) AutoIncrement() (bool, bool) { return false, false }

func (ct crdbColumnType) Length() (int64, bool) { return 0, false }

func (ct crdbColumnType) DecimalSize() (int64, int64, bool) { return 0, 0, false }

func (ct crdbColumnType) Nullable() (bool, bool) { return true, true }

func (ct crdbColumnType) Unique() (bool, bool) { return false, false }

func (ct crdbColumnType) ScanType() reflect.Type { return reflect.TypeOf("") }

func (ct crdbColumnType) Comment() (string, bool) { return "", false }

func (ct crdbColumnType) DefaultValue() (string, bool) { return "", false }

// migrateJSONColumn 用真实 postgres migrator 跑一遍 AutoMigrate 的列比对，
// databaseTypeName 是库端回报的类型名，shimmed 决定是否套上 CRDB 适配。
func migrateJSONColumn(t *testing.T, databaseTypeName string, shimmed bool) []string {
	t.Helper()
	dsn := "host=127.0.0.1 port=1 user=dujiao dbname=dujiao sslmode=disable"
	dialector, err := newDialector("cockroach", dsn)
	if err != nil {
		t.Fatalf("newDialector: %v", err)
	}
	db, err := gorm.Open(dialector, &gorm.Config{DryRun: true, DisableAutomaticPing: true})
	if err != nil {
		t.Fatalf("open cockroach db: %v", err)
	}

	// recorder 内层保持真实 postgres migrator，别名表缺什么就照原样暴露什么
	recorder := &alterColumnRecorder{Migrator: dialector.(cockroachDialector).Dialector.Migrator(db)}
	var resolved gorm.Migrator = recorder
	if shimmed {
		resolved = cockroachMigrator{Migrator: recorder}
	}
	db.Dialector = crdbMigratorStub{Dialector: dialector, migrator: resolved}

	stmt := &gorm.Statement{DB: db}
	if err := stmt.Parse(&crdbJSONFixture{}); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	columnType := crdbColumnType{name: "detail_json", databaseTypeName: databaseTypeName}
	base := migrator.Migrator{Config: migrator.Config{DB: db, Dialector: dialector}}
	if err := base.MigrateColumn(&crdbJSONFixture{}, stmt.Schema.FieldsByDBName["detail_json"], columnType); err != nil {
		t.Fatalf("migrate column: %v", err)
	}
	return recorder.altered
}

func TestCockroachMigratorStopsRetypingJSONColumns(t *testing.T) {
	// CRDB 里 JSON 是 JSONB 的别名，声明成 json 的列 introspection 一律回报 JSONB
	if got := migrateJSONColumn(t, "JSONB", true); len(got) != 0 {
		t.Fatalf("jsonb column must not be retyped on every boot, altered=%v", got)
	}

	// 对照组：去掉适配后同一列仍被判定需要重写，正是每次重启那批 ALTER 的来源
	if got := migrateJSONColumn(t, "JSONB", false); len(got) != 1 {
		t.Fatalf("without the json alias the json/jsonb mismatch must trigger a rewrite, altered=%v", got)
	}

	// 别名互认不能变成整体跳过：类型确实不同仍需重写
	if got := migrateJSONColumn(t, "text", true); len(got) != 1 {
		t.Fatalf("text column must still be retyped to json, altered=%v", got)
	}
}

func TestCockroachMigratorTypeAliasesDelegateToPostgres(t *testing.T) {
	dsn := "host=127.0.0.1 port=1 user=dujiao dbname=dujiao sslmode=disable"
	dialector, err := newDialector("cockroach", dsn)
	if err != nil {
		t.Fatalf("newDialector: %v", err)
	}
	db, err := gorm.Open(dialector, &gorm.Config{DryRun: true, DisableAutomaticPing: true})
	if err != nil {
		t.Fatalf("open cockroach db: %v", err)
	}
	migratorDB := cockroachMigrator{Migrator: dialector.(cockroachDialector).Dialector.Migrator(db)}

	if aliases := migratorDB.GetTypeAliases("JSONB"); !hasTypeAlias(aliases, "json") {
		t.Fatalf("jsonb must accept json, aliases=%v", aliases)
	}
	if aliases := migratorDB.GetTypeAliases("json"); !hasTypeAlias(aliases, "jsonb") {
		t.Fatalf("json must accept jsonb, aliases=%v", aliases)
	}
	// 其余类型沿用 postgres 驱动的别名表
	if aliases := migratorDB.GetTypeAliases("int8"); !hasTypeAlias(aliases, "bigint") {
		t.Fatalf("int8 must delegate to the postgres alias table, aliases=%v", aliases)
	}
}

func hasTypeAlias(aliases []string, want string) bool {
	for _, alias := range aliases {
		if alias == want {
			return true
		}
	}
	return false
}
