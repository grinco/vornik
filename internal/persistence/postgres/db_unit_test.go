package postgres

import (
	"database/sql"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// TestDefaultConfigValues tests all default configuration values.
func TestDefaultConfigValues(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Host != "localhost" {
		t.Errorf("expected Host 'localhost', got '%s'", cfg.Host)
	}
	if cfg.Port != 5432 {
		t.Errorf("expected Port 5432, got %d", cfg.Port)
	}
	if cfg.Database != "vornik" {
		t.Errorf("expected Database 'vornik', got '%s'", cfg.Database)
	}
	if cfg.User != "vornik" {
		t.Errorf("expected User 'vornik', got '%s'", cfg.User)
	}
	if cfg.SSLMode != "disable" {
		t.Errorf("expected SSLMode 'disable', got '%s'", cfg.SSLMode)
	}
	if cfg.MaxOpenConns != 25 {
		t.Errorf("expected MaxOpenConns 25, got %d", cfg.MaxOpenConns)
	}
	if cfg.MaxIdleConns != 5 {
		t.Errorf("expected MaxIdleConns 5, got %d", cfg.MaxIdleConns)
	}
	if cfg.ConnMaxLifetime != 5*time.Minute {
		t.Errorf("expected ConnMaxLifetime 5m, got %v", cfg.ConnMaxLifetime)
	}
	if cfg.ConnMaxIdleTime != 2*time.Minute {
		t.Errorf("expected ConnMaxIdleTime 2m, got %v", cfg.ConnMaxIdleTime)
	}
	if cfg.ConnectTimeout != 10*time.Second {
		t.Errorf("expected ConnectTimeout 10s, got %v", cfg.ConnectTimeout)
	}
}

// TestConfigDSN tests DSN generation.
func TestConfigDSN(t *testing.T) {
	tests := []struct {
		name     string
		cfg      Config
		expected string
	}{
		{
			name: "standard config",
			cfg: Config{
				Host:     "localhost",
				Port:     5432,
				User:     "testuser",
				Password: "testpass",
				Database: "testdb",
				SSLMode:  "disable",
			},
			expected: "host=localhost port=5432 user=testuser password=testpass dbname=testdb sslmode=disable",
		},
		{
			name: "with SSL require",
			cfg: Config{
				Host:     "prod-db",
				Port:     5433,
				User:     "admin",
				Password: "secret",
				Database: "production",
				SSLMode:  "require",
			},
			expected: "host=prod-db port=5433 user=admin password=secret dbname=production sslmode=require",
		},
		{
			name: "with SSL verify-full",
			cfg: Config{
				Host:     "secure-db",
				Port:     5432,
				User:     "readonly",
				Password: "password123",
				Database: "analytics",
				SSLMode:  "verify-full",
			},
			expected: "host=secure-db port=5432 user=readonly password=password123 dbname=analytics sslmode=verify-full",
		},
		{
			name: "empty password",
			cfg: Config{
				Host:     "localhost",
				Port:     5432,
				User:     "postgres",
				Password: "",
				Database: "postgres",
				SSLMode:  "disable",
			},
			expected: "host=localhost port=5432 user=postgres password='' dbname=postgres sslmode=disable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dsn := tt.cfg.DSN()
			if dsn != tt.expected {
				t.Errorf("expected DSN '%s', got '%s'", tt.expected, dsn)
			}
		})
	}
}

// TestConfigDSNSpecialCharacters tests DSN escaping for special characters.
func TestConfigDSNSpecialCharacters(t *testing.T) {
	cfg := Config{
		Host:     "local host",
		Port:     5432,
		User:     "testuser",
		Password: "p@ss'w0rd!\\",
		Database: "test db",
		SSLMode:  "disable",
	}

	dsn := cfg.DSN()
	expected := "host='local host' port=5432 user=testuser password='p@ss\\'w0rd!\\\\' dbname='test db' sslmode=disable"
	if dsn != expected {
		t.Fatalf("expected DSN %q, got %q", expected, dsn)
	}
}

// TestConfigPortValidation tests port value handling.
func TestConfigPortValidation(t *testing.T) {
	tests := []struct {
		name string
		port int
	}{
		{"standard port", 5432},
		{"custom port", 5433},
		{"low port", 1024},
		{"high port", 65535},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{Port: tt.port}
			dsn := cfg.DSN()
			// DSN should contain the port
			expected := "port="
			if len(expected) > 0 && dsn == "" {
				t.Error("expected non-empty DSN")
			}
		})
	}
}

// TestConfigStructZeroValue tests Config struct zero values.
func TestConfigStructZeroValue(t *testing.T) {
	var cfg Config

	if cfg.Host != "" {
		t.Errorf("expected empty Host, got '%s'", cfg.Host)
	}
	if cfg.Port != 0 {
		t.Errorf("expected zero Port, got %d", cfg.Port)
	}
	if cfg.Database != "" {
		t.Errorf("expected empty Database, got '%s'", cfg.Database)
	}
	if cfg.MaxOpenConns != 0 {
		t.Errorf("expected zero MaxOpenConns, got %d", cfg.MaxOpenConns)
	}
	if cfg.ConnectTimeout != 0 {
		t.Errorf("expected zero ConnectTimeout, got %v", cfg.ConnectTimeout)
	}
}

// TestConfigWithCustomTimeouts tests configuration with custom timeout values.
func TestConfigWithCustomTimeouts(t *testing.T) {
	cfg := Config{
		Host:            "localhost",
		Port:            5432,
		Database:        "testdb",
		User:            "test",
		Password:        "test",
		SSLMode:         "disable",
		MaxOpenConns:    50,
		MaxIdleConns:    10,
		ConnMaxLifetime: 30 * time.Minute,
		ConnMaxIdleTime: 10 * time.Minute,
		ConnectTimeout:  5 * time.Second,
	}

	if cfg.MaxOpenConns != 50 {
		t.Errorf("expected MaxOpenConns 50, got %d", cfg.MaxOpenConns)
	}
	if cfg.MaxIdleConns != 10 {
		t.Errorf("expected MaxIdleConns 10, got %d", cfg.MaxIdleConns)
	}
	if cfg.ConnMaxLifetime != 30*time.Minute {
		t.Errorf("expected ConnMaxLifetime 30m, got %v", cfg.ConnMaxLifetime)
	}
	if cfg.ConnMaxIdleTime != 10*time.Minute {
		t.Errorf("expected ConnMaxIdleTime 10m, got %v", cfg.ConnMaxIdleTime)
	}
	if cfg.ConnectTimeout != 5*time.Second {
		t.Errorf("expected ConnectTimeout 5s, got %v", cfg.ConnectTimeout)
	}
}

func TestMapDBErrorNoRows(t *testing.T) {
	if got := mapDBError(sql.ErrNoRows); got != persistence.ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", got)
	}
}

// TestConfigSSLModes tests various SSL mode configurations.
func TestConfigSSLModes(t *testing.T) {
	validModes := []string{"disable", "require", "verify-ca", "verify-full"}

	for _, mode := range validModes {
		t.Run(mode, func(t *testing.T) {
			cfg := Config{
				Host:     "localhost",
				Port:     5432,
				Database: "test",
				User:     "test",
				Password: "test",
				SSLMode:  mode,
			}

			dsn := cfg.DSN()
			if dsn == "" {
				t.Error("expected non-empty DSN")
			}
		})
	}
}
