package queue

import (
	"testing"

	"github.com/dujiao-next/internal/config"
)

func TestBuildRedisOptCarriesACLUsername(t *testing.T) {
	opt := buildRedisOpt(&config.QueueConfig{
		Host:     "redis.internal",
		Port:     6380,
		Username: "dujiao",
		Password: "s3cret",
		DB:       3,
	})
	if got, want := opt.Addr, "redis.internal:6380"; got != want {
		t.Fatalf("addr = %q, want %q", got, want)
	}
	if opt.Username != "dujiao" {
		t.Fatalf("username = %q, want dujiao", opt.Username)
	}
	if opt.Password != "s3cret" || opt.DB != 3 {
		t.Fatalf("credentials lost: password=%q db=%d", opt.Password, opt.DB)
	}
}
