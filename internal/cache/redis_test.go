package cache

import (
	"testing"

	"github.com/dujiao-next/internal/config"
)

func TestInitRedisCarriesACLUsername(t *testing.T) {
	t.Cleanup(func() {
		redisClient = nil
		redisEnabled = false
	})

	if err := InitRedis(&config.RedisConfig{
		Enabled:  true,
		Host:     "127.0.0.1",
		Port:     6379,
		Username: "dujiao",
		Password: "s3cret",
		DB:       2,
		Prefix:   "dj",
	}); err != nil {
		t.Fatalf("init redis: %v", err)
	}
	opt := redisClient.Options()
	if opt.Username != "dujiao" {
		t.Fatalf("username = %q, want dujiao", opt.Username)
	}
	if opt.Password != "s3cret" || opt.DB != 2 {
		t.Fatalf("credentials lost: password=%q db=%d", opt.Password, opt.DB)
	}
}
