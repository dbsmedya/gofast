package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dbsmedya/gofast/pkg/api"
	"github.com/dbsmedya/gofast/pkg/config"
	"github.com/dbsmedya/gofast/pkg/parser"
	"github.com/dbsmedya/gofast/pkg/storage"
	"github.com/spf13/cobra"
)

var (
	cfgFile      string
	slowLogDir   string
	duckDBPath   string
	configLoaded *config.Config

	// version is stamped at build time via -ldflags "-X main.version=<tag>";
	// it stays "dev" for plain `go build`.
	version = "dev"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "gofast-cli",
		Short: "GoFast MySQL Slow Log Parser CLI",
		Long:  `A CLI tool for parsing MySQL slow logs and storing them in DuckDB`,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// Load config before each command
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			// Override with CLI flags if provided
			if slowLogDir != "" {
				cfg.Parser.SlowLogDir = slowLogDir
			}
			if duckDBPath != "" {
				cfg.DuckDB.Path = duckDBPath
			}

			configLoaded = cfg
			return nil
		},
	}

	// Global flags
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is ./config.yaml)")
	rootCmd.PersistentFlags().StringVar(&slowLogDir, "slow-log-dir", "", "directory containing slow log files")
	rootCmd.PersistentFlags().StringVar(&duckDBPath, "duck-db-path", "", "path to DuckDB database file")

	// Add commands
	rootCmd.AddCommand(parseCmd())
	rootCmd.AddCommand(statsCmd())
	rootCmd.AddCommand(queryCmd())
	rootCmd.AddCommand(versionCmd())
	rootCmd.AddCommand(serveCmd())

	// Execute
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func parseCmd() *cobra.Command {
	var (
		singleFile string
		verbose    bool
	)

	cmd := &cobra.Command{
		Use:   "parse",
		Short: "Parse MySQL slow logs",
		Long:  `Parse MySQL slow log files and store them in DuckDB`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if configLoaded == nil {
				return fmt.Errorf("config not loaded")
			}

			// Setup context with cancellation
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// Handle interrupt signals
			sigChan := make(chan os.Signal, 1)
			signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
			go func() {
				<-sigChan
				fmt.Println("\nReceived interrupt signal, shutting down...")
				cancel()
			}()

			// Open storage in read-write mode for parsing (needs to insert data)
			store, err := storage.NewStorage(configLoaded.GetDSN(), false)
			if err != nil {
				return fmt.Errorf("failed to open storage: %w", err)
			}
			defer store.Close()

			// Drop indexes for faster bulk insert
			if err := store.DropSlowLogIndexes(); err != nil {
				return fmt.Errorf("failed to drop indexes: %w", err)
			}

			// Create parser engine
			engine := parser.NewEngine(store, configLoaded.Parser.BatchSize,
				configLoaded.Parser.FilePatterns, configLoaded.Parser.Workers)

			var result *parser.ParseResult
			startTime := time.Now()

			if singleFile != "" {
				// Parse single file
				if verbose {
					fmt.Printf("Parsing single file: %s\n", singleFile)
				}
				result, err = engine.ParseFile(ctx, singleFile)
			} else {
				// Parse directory
				if verbose {
					fmt.Printf("Parsing directory: %s\n", configLoaded.Parser.SlowLogDir)
				}
				result, err = engine.ParseDirectory(ctx, configLoaded.Parser.SlowLogDir)
			}

			// Dedupe + indexes after bulk load [D6]
			if finErr := store.FinalizeIngestion(ctx); finErr != nil {
				if err != nil {
					return fmt.Errorf("parsing failed: %v; additionally, finalization failed: %w", err, finErr)
				}
				return fmt.Errorf("failed to finalize after parse: %w", finErr)
			}

			if err != nil {
				return fmt.Errorf("parsing failed: %w", err)
			}

			// Print results
			fmt.Println("\n=== Parse Results ===")
			fmt.Printf("Files processed: %d\n", result.FilesProcessed)
			fmt.Printf("Files skipped:   %d\n", result.FilesSkipped)
			fmt.Printf("Files failed:    %d\n", result.FilesFailed)
			fmt.Printf("Entries parsed:  %d\n", result.EntriesParsed)
			fmt.Printf("Entries stored:  %d\n", result.EntriesStored)
			fmt.Printf("Duration:        %s\n", result.Duration())

			if len(result.Errors) > 0 {
				fmt.Printf("\nErrors (%d):\n", len(result.Errors))
				for _, msg := range result.Errors {
					fmt.Printf("  - %s\n", msg)
				}
			}

			if verbose {
				fmt.Printf("\nTotal time: %s\n", time.Since(startTime))
			}

			return nil
		},
	}

	cmd.Flags().StringVarP(&singleFile, "file", "f", "", "parse a single file instead of directory")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "verbose output")

	return cmd
}

func statsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stats",
		Short: "Show database statistics",
		Long:  `Show statistics about stored slow logs`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if configLoaded == nil {
				return fmt.Errorf("config not loaded")
			}

			ctx := context.Background()

			// Open storage in read-only mode for stats
			store, err := storage.NewStorage(configLoaded.GetDSN(), true)
			if err != nil {
				return fmt.Errorf("failed to open storage: %w", err)
			}
			defer store.Close()

			// Get stats
			stats, err := store.GetStats(ctx)
			if err != nil {
				return fmt.Errorf("failed to get stats: %w", err)
			}

			// Get file stats
			fileStats, err := store.GetFileStats(ctx)
			if err != nil {
				return fmt.Errorf("failed to get file stats: %w", err)
			}

			fmt.Println("=== Slow Log Statistics ===")
			for key, value := range stats {
				fmt.Printf("%s: %v\n", key, value)
			}

			fmt.Println("\n=== File Statistics ===")
			for key, value := range fileStats {
				fmt.Printf("%s: %v\n", key, value)
			}

			return nil
		},
	}
}

func queryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "query [SQL]",
		Short: "Execute a SQL query",
		Long:  `Execute a raw SQL query against the DuckDB database`,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if configLoaded == nil {
				return fmt.Errorf("config not loaded")
			}

			ctx := context.Background()
			sql := args[0]

			// Open storage in read-only mode for queries
			store, err := storage.NewStorage(configLoaded.GetDSN(), true)
			if err != nil {
				return fmt.Errorf("failed to open storage: %w", err)
			}
			defer store.Close()

			// Execute query
			result, err := store.Query(ctx, sql)
			if err != nil {
				return fmt.Errorf("query failed: %w", err)
			}

			// Print results
			fmt.Printf("\nColumns: %v\n", result.Columns)
			fmt.Printf("Rows (%d):\n", result.Count)
			for i, row := range result.Rows {
				fmt.Printf("  %d: %v\n", i+1, row)
			}

			return nil
		},
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("GoFast CLI %s\n", version)
		},
	}
}

// fixedStoreProvider wraps a single, never-swapped *storage.Storage so it
// satisfies api.StoreProvider. `serve` never parses (and so never swaps the
// store); the RWMutex + nil check keep it a faithful StoreProvider for a
// store that could, in principle, be swapped under a write lock.
type fixedStoreProvider struct {
	mu sync.RWMutex
	s  *storage.Storage
}

func (p *fixedStoreProvider) WithStore(fn func(*storage.Storage) error) error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.s == nil {
		return api.ErrStoreUnavailable
	}
	return fn(p.s)
}

// buildServeRouter opens a read-only storage handle over cfg's DuckDB path and
// builds the machine-API router (pkg/api) around it. It returns the handler,
// a cleanup func to close the store, and any error opening storage. serve
// never parses, so storage is opened read-only.
func buildServeRouter(cfg *config.Config, token string, disableAuth bool) (http.Handler, func() error, error) {
	store, err := storage.NewStorage(cfg.GetDSN(), true)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open storage: %w", err)
	}

	provider := &fixedStoreProvider{s: store}

	handler := api.NewRouter(provider, api.Options{
		Token:       token,
		DisableAuth: disableAuth,
		// DisableSQLExecute left false: /sql/execute stays enabled.
	})

	return handler, store.Close, nil
}

func serveCmd() *cobra.Command {
	var (
		port   int
		noAuth bool
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve the machine API",
		Long:  `Serve the read-only machine API (pkg/api) over HTTP`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if configLoaded == nil {
				return fmt.Errorf("config not loaded")
			}
			cfg := configLoaded

			token := strings.TrimSpace(cfg.API.APIKey)
			if token == "" {
				token = strings.TrimSpace(os.Getenv("GOFAST_API_KEY"))
			}

			if token == "" && !noAuth {
				return fmt.Errorf("serve: auth enabled but no token — set GOFAST_API_KEY or pass --no-auth")
			}

			if port != 0 {
				cfg.API.Port = port
			}

			handler, cleanup, err := buildServeRouter(cfg, token, noAuth)
			if err != nil {
				return err
			}
			defer func() { _ = cleanup() }()

			addr := cfg.GetAPIAddr()
			srv := &http.Server{
				Addr:    addr,
				Handler: handler,
			}

			// Graceful shutdown
			go func() {
				sigChan := make(chan os.Signal, 1)
				signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
				<-sigChan

				fmt.Println("\nReceived interrupt signal, shutting down...")
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = srv.Shutdown(ctx)
			}()

			fmt.Printf("starting API server on %s\n", addr)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				return fmt.Errorf("server error: %w", err)
			}

			return nil
		},
	}

	cmd.Flags().IntVar(&port, "port", 0, "port to serve on (overrides config)")
	cmd.Flags().BoolVar(&noAuth, "no-auth", false, "DEV ONLY: disable bearer auth on /sql/*")

	return cmd
}
