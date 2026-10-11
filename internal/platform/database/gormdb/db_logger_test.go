package gormdb

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
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

func TestNewDialectorAcceptsOnlySupportedDrivers(t *testing.T) {
	dsn := "host=127.0.0.1 port=1 user=dujiao dbname=dujiao sslmode=disable"
	for _, driver := range []string{"postgres", "PostgreSQL"} {
		dialector, err := newDialector(driver, dsn)
		if err != nil {
			t.Fatalf("newDialector(%s): %v", driver, err)
		}
		if got := dialector.Name(); got != "postgres" {
			t.Fatalf("newDialector(%s) name = %q, want postgres", driver, got)
		}
	}
	for _, driver := range []string{"", "sqlite"} {
		dialector, err := newDialector(driver, "file:dujiao.db")
		if err != nil {
			t.Fatalf("newDialector(%s): %v", driver, err)
		}
		if got := dialector.Name(); got != "sqlite" {
			t.Fatalf("newDialector(%s) name = %q, want sqlite", driver, got)
		}
	}
	// 未在支持列表里的驱动（含 CockroachDB）必须直接失败，不能静默回落
	for _, driver := range []string{"mysql", "cockroach", "cockroachdb"} {
		if _, err := newDialector(driver, dsn); err == nil {
			t.Fatalf("unsupported database driver %q must fail", driver)
		}
	}
}
