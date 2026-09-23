// Command aiproxy runs the aiproxy HTTP proxy.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/al-maisan/aiproxy/internal/config"
	"github.com/al-maisan/aiproxy/internal/keys"
	"github.com/al-maisan/aiproxy/internal/proxy"
	"github.com/al-maisan/aiproxy/internal/version"
)

// keyCacheTTL bounds how long an OpenCode auth-file key is cached.
const keyCacheTTL = 30 * time.Second

// serverIdleTimeout closes idle keep-alive connections from clients.
const serverIdleTimeout = 120 * time.Second

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "aiproxy: "+err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("aiproxy", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		configPath  = fs.String("config", "", "path to config file (default: first found in standard locations)")
		listen      = fs.String("listen", "", "override listen address (host:port)")
		showVersion = fs.Bool("version", false, "print version and exit")
		printConfig = fs.Bool("print-config", false, "print the default config to stdout and exit")
		initConfig  = fs.Bool("init", false, "write the default config to the -config path (or the user config location) and exit")
		force       = fs.Bool("force", false, "overwrite an existing file with -init")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	switch {
	case *showVersion:
		_, err := fmt.Fprintln(stdout, version.String())
		return err
	case *printConfig:
		_, err := io.WriteString(stdout, config.DefaultTOML)
		return err
	case *initConfig:
		target := *configPath
		if target == "" {
			target = defaultUserConfigPath()
		}
		return writeDefaultConfig(target, *force, stdout)
	}

	path, err := resolveConfigPath(*configPath)
	if err != nil {
		return err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	if *listen != "" {
		cfg.Listen = *listen
	}

	logger := newLogger(cfg.Log)
	logger.Info("configuration loaded", "path", path, "version", version.Version)

	p, err := proxy.New(cfg, logger, keys.NewResolver(keys.DefaultAuthPath(), keyCacheTTL))
	if err != nil {
		return fmt.Errorf("initialize proxy: %w", err)
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           p.Handler(),
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Std(),
		IdleTimeout:       serverIdleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.Listen)
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("server: %w", err)
		}
		return nil
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout.Std())
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return nil
	}
}

func newLogger(cfg config.Log) *slog.Logger {
	var level slog.Level
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if cfg.Format == "json" {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}
	return slog.New(handler)
}

func resolveConfigPath(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil { //nolint:gosec // operator-supplied path
			return "", fmt.Errorf("config: %w", err)
		}
		return explicit, nil
	}
	if env := os.Getenv("AIPROXY_CONFIG"); env != "" {
		if _, err := os.Stat(env); err != nil { //nolint:gosec // operator-supplied path
			return "", fmt.Errorf("config from AIPROXY_CONFIG: %w", err)
		}
		return env, nil
	}

	var candidates []string
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		candidates = append(candidates, filepath.Join(xdg, "aiproxy", "config.toml"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".config", "aiproxy", "config.toml"))
	}
	candidates = append(candidates, "/etc/aiproxy/config.toml", "aiproxy.toml")

	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil { //nolint:gosec // operator-supplied path
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no config file found (looked in %s); pass -config", strings.Join(candidates, ", "))
}

func defaultUserConfigPath() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "aiproxy", "config.toml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "aiproxy.toml"
	}
	return filepath.Join(home, ".config", "aiproxy", "config.toml")
}

func writeDefaultConfig(path string, force bool, stdout io.Writer) error {
	if !force {
		if _, err := os.Stat(path); err == nil { //nolint:gosec // operator-supplied path
			return fmt.Errorf("config %s already exists (use -force to overwrite)", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	}
	if err := os.WriteFile(path, []byte(config.DefaultTOML), 0o600); err != nil { //nolint:gosec // operator-supplied path
		return err
	}
	_, err := fmt.Fprintf(stdout, "wrote default config to %s\n", path)
	return err
}
