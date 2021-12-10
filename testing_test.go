package dbw

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_getTestOpts(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	t.Run("WithTestMigration", func(t *testing.T) {
		fn := func(context.Context, string, string) error { return nil }
		opts := getTestOpts(WithTestMigration(fn))
		testOpts := getDefaultTestOptions()
		testOpts.withTestMigration = fn
		assert.NotNil(opts, testOpts.withTestMigration)
	})
	t.Run("WithTestMigration", func(t *testing.T) {
		opts := getTestOpts(WithTestDatabaseUrl("url"))
		testOpts := getDefaultTestOptions()
		testOpts.withTestDatabaseUrl = "url"
		assert.Equal(opts, testOpts)
	})
}

func Test_TestSetup(t *testing.T) {
	testMigrationFn := func(context.Context, string, string) error {
		conn, err := Open(Sqlite, "file::memory:")
		require.NoError(t, err)
		rw := New(conn)
		_, err = rw.Exec(context.Background(), testQueryCreateTablesSqlite, nil)
		require.NoError(t, err)
		return nil
	}
	tests := []struct {
		name     string
		dialect  string
		opt      []TestOption
		validate func() bool
	}{
		{
			name:    "with-migration",
			dialect: "sqlite",
			opt:     []TestOption{WithTestDialect(Sqlite.String()), WithTestMigration(testMigrationFn)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			TestSetup(t, tt.opt...)
			if tt.validate != nil {
				assert.True(tt.validate())
			}
		})
	}
}

func Test_CreateDropTestTables(t *testing.T) {
	t.Run("execute", func(t *testing.T) {
		db, _ := TestSetup(t, WithTestDialect(Sqlite.String()))
		testDropTables(t, db)
		TestCreateTables(t, db)
	})
}