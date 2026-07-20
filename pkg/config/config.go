package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/mitchellh/mapstructure"
	"github.com/spf13/viper"
)

// defaultFilePatterns reproduces the historical discovery filter (contains slow /
// ends with .log / contains mysql) via case-insensitive globs [L4].
var defaultFilePatterns = []string{"*slow*", "*.log", "*mysql*"}

// Config represents the application configuration
type Config struct {
	DuckDB struct {
		Path string `mapstructure:"path"`
	} `mapstructure:"duckdb"`

	Parser struct {
		SlowLogDir   string   `mapstructure:"slow_log_dir"`
		BatchSize    int      `mapstructure:"batch_size"`
		FilePatterns []string `mapstructure:"file_patterns"`
		Workers      int      `mapstructure:"workers"`
	} `mapstructure:"parser"`

	API struct {
		Host   string `mapstructure:"host"`
		Port   int    `mapstructure:"port"`
		APIKey string `mapstructure:"api_key"`
	} `mapstructure:"api"`
}

// Load loads configuration from file and environment variables
func Load(configPath string) (*Config, error) {
	v := viper.New()

	// Set defaults
	v.SetDefault("duckdb.path", "gofast.duckdb")
	v.SetDefault("parser.slow_log_dir", "./logs")
	v.SetDefault("parser.batch_size", 1000)
	v.SetDefault("parser.file_patterns", defaultFilePatterns)
	defaultWorkers := 4
	if n := runtime.NumCPU(); n < defaultWorkers {
		defaultWorkers = n
	}
	if defaultWorkers < 1 {
		defaultWorkers = 1
	}
	v.SetDefault("parser.workers", defaultWorkers) // [L13] resolved once at Load
	v.SetDefault("api.host", "0.0.0.0")
	v.SetDefault("api.port", 8080)
	v.SetDefault("api.api_key", "")

	// Environment variables
	v.SetEnvPrefix("GOFAST")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	_ = v.BindEnv("api.api_key", "GOFAST_API_KEY", "GOFAST_API_API_KEY")
	_ = v.BindEnv("parser.file_patterns", "GOFAST_PARSER_FILE_PATTERNS")
	_ = v.BindEnv("parser.workers", "GOFAST_PARSER_WORKERS")
	v.AutomaticEnv()

	// Config file
	if configPath != "" {
		v.SetConfigFile(configPath)
	} else {
		v.SetConfigName("config")
		v.SetConfigType("yaml")
		v.AddConfigPath(".")
		v.AddConfigPath("./config")
		v.AddConfigPath("/etc/gofast")
	}

	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("error reading config file: %w", err)
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg, viper.DecodeHook(
		mapstructure.ComposeDecodeHookFunc(
			mapstructure.StringToSliceHookFunc(","),
			mapstructure.StringToTimeDurationHookFunc(),
		),
	)); err != nil {
		return nil, fmt.Errorf("error unmarshaling config: %w", err)
	}

	// Empty patterns → default [L4]
	if len(cfg.Parser.FilePatterns) == 0 {
		cfg.Parser.FilePatterns = append([]string(nil), defaultFilePatterns...)
	}

	// Validate globs [L4]
	for _, p := range cfg.Parser.FilePatterns {
		if _, err := filepath.Match(strings.ToLower(p), "x"); err != nil {
			return nil, fmt.Errorf("invalid parser.file_patterns glob %q: %w", p, err)
		}
	}

	// Workers: clamp to at least 1; default already set at Load [L13]
	if cfg.Parser.Workers < 1 {
		cfg.Parser.Workers = 1
	}

	// Ensure DuckDB directory exists
	dbDir := filepath.Dir(cfg.DuckDB.Path)
	if dbDir != "." && dbDir != "" {
		if err := os.MkdirAll(dbDir, 0755); err != nil {
			return nil, fmt.Errorf("error creating duckdb directory: %w", err)
		}
	}

	return &cfg, nil
}

// DefaultFilePatterns returns a copy of the default discovery patterns.
func DefaultFilePatterns() []string {
	return append([]string(nil), defaultFilePatterns...)
}

// GetDSN returns the DuckDB connection string
func (c *Config) GetDSN() string {
	return c.DuckDB.Path
}

// GetAPIAddr returns the API server address
func (c *Config) GetAPIAddr() string {
	return fmt.Sprintf("%s:%d", c.API.Host, c.API.Port)
}
