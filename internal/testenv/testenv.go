// Package testenv finds the external services some tests need.
package testenv

import (
	"os"
	"testing"
)

// The environment variables that point tests at real services.
const (
	PostgresURL = "TAPIRPOST_TEST_POSTGRES_URL"
	RedisURL    = "TAPIRPOST_TEST_REDIS_URL"
	// Require turns a missing service from a skip into a failure. CI sets
	// it, so that a broken service container cannot pass as "skipped".
	Require = "TAPIRPOST_TEST_REQUIRE_SERVICES"
)

// Service returns the value of the named variable. Without one the test is
// skipped, or failed when Require is set.
func Service(t testing.TB, name string) string {
	t.Helper()
	if url := os.Getenv(name); url != "" {
		return url
	}
	if os.Getenv(Require) != "" {
		t.Fatalf("%s is not set, and %s demands it", name, Require)
	}
	t.Skipf("set %s to run this test against a real service", name)
	return ""
}
