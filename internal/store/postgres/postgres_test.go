package postgres_test

import (
	"testing"

	"github.com/zieludev/tapirpost/internal/content"
	"github.com/zieludev/tapirpost/internal/store/postgres/postgrestest"
	"github.com/zieludev/tapirpost/internal/store/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) content.Store {
		return postgrestest.Open(t)
	})
}
