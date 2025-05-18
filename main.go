package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"github.com/caarlos0/env/v11"
	"github.com/getsentry/sentry-go"
	sentryslog "github.com/getsentry/sentry-go/slog"
	slogmulti "github.com/samber/slog-multi"
	"io/fs"
	"io/ioutil"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

//go:embed web/*
var webFS embed.FS

func main() {
	ctx := context.Background()
	cfg, err := env.ParseAs[RunCfg]()
	if err != nil {
		_ = fmt.Sprintf("env: %+v\n", err)
		os.Exit(1)
	}
	if err := run(ctx, cfg); err != nil {
		_ = fmt.Sprintf("run: %+v\n", err)
		os.Exit(1)
	}
}

type RunCfg struct {
	Host           string `env:"HOST" envDefault:"0.0.0.0"`
	Port           string `env:"PORT" envDefault:"8080"`
	StaticDir      string `env:"STATIC_DIR"`
	StaticURL      string `env:"STATIC_URL" envDefault:"http://localhost:8080/"`
	SentryDsn      string `env:"SENTRY_DSN"`
	DiscordToken   string `env:"DISCORD_TOKEN,required"`
	CobaltEndpoint string `env:"COBALT_ENDPOINT"`
	CobaltAPIKey   string `env:"COBALT_API_KEY"`
}

func run(ctx context.Context, cfg RunCfg) error {
	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	err := sentry.Init(sentry.ClientOptions{
		Dsn: cfg.SentryDsn,
	})
	if err != nil {
		return fmt.Errorf("sentry.Init: %w", err)
	}
	defer sentry.Flush(2 * time.Second)

	logger := slog.New(slogmulti.Fanout(
		slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{AddSource: true}),
		sentryslog.Option{Level: slog.LevelWarn}.NewSentryHandler(),
	))

	slog.SetDefault(logger)

	mime.AddExtensionType(".mp4", "video/mp4")
	mime.AddExtensionType(".jpg", "image/jpeg")
	mime.AddExtensionType(".jpeg", "image/jpeg")
	mime.AddExtensionType(".png", "image/png")
	mime.AddExtensionType(".gif", "image/gif")
	mime.AddExtensionType(".webp", "image/webp")
	mime.AddExtensionType(".webm", "video/webm")
	mime.AddExtensionType(".mp3", "audio/mpeg")
	mime.AddExtensionType(".wav", "audio/wav")
	mime.AddExtensionType(".ogg", "audio/ogg")

	dl := &DownloaderRegistry{}
	dl.Add(&CobaltDownloader{
		Dir:      cfg.StaticDir,
		Endpoint: cfg.CobaltEndpoint,
		APIKey:   cfg.CobaltAPIKey,
	})
	dl.Add(&SsvidDownloader{
		Dir: cfg.StaticDir,
	})

	bot := &Bot{
		PublicURL:   cfg.StaticURL,
		Downloaders: dl,
	}

	job(ctx, "delete-old-previews", time.Minute, time.Hour, func() error {
		return deleteOldPreviews(cfg.StaticDir, 20)
	})

	err = bot.Start(cfg.DiscordToken)
	if err != nil {
		return fmt.Errorf("bot.Start: %w", err)
	}
	defer bot.Stop()

	subFS, err := fs.Sub(webFS, "web")
	if err != nil {
		return fmt.Errorf("fs.Sub: %w", err)
	}

	httpServer := &http.Server{
		Addr:              cfg.Host + ":" + cfg.Port,
		Handler:           files(http.FileServer(&mergeFS{http.FS(subFS), http.Dir(cfg.StaticDir)})),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errChan := make(chan error, 1)
	go func() {
		slog.InfoContext(ctx, "httpServer started", "addr", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errChan <- err
		}
	}()

	http.FS(webFS)

	select {
	case err := <-errChan:
		return err
	case <-ctx.Done():
		slog.InfoContext(ctx, "shutting down httpServer")
	}

	ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return httpServer.Shutdown(ctx)
}

func files(next http.Handler) http.Handler {
	return http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
		if req.Method != "GET" && req.Method != "HEAD" {
			res.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		// Prevent directory listings
		if req.URL.Path != "/" && strings.HasSuffix(req.URL.Path, "/") {
			http.NotFound(res, req)
			return
		}

		if strings.HasSuffix(req.URL.Path, ".mp4") {
			res.Header().Set("Content-Type", "video/mp4")
			res.Header().Set("Cache-Control", "public, max-age=31536000")
		} else {
			res.Header().Set("Cache-Control", "public, max-age=3600")
		}

		next.ServeHTTP(res, req)
	})
}

func job(ctx context.Context, slug string, timeout, interval time.Duration, jobFunc func() error) {
	go func() {
		hub := sentry.CurrentHub().Clone()
		hub.ConfigureScope(func(scope *sentry.Scope) {
			scope.SetContext("monitor", sentry.Context{"slug": slug})
		})
		time.Sleep(timeout)
		for {
			hub.CaptureCheckIn(&sentry.CheckIn{
				MonitorSlug: slug,
				Status:      sentry.CheckInStatusInProgress,
			}, nil)
			if err := jobFunc(); err != nil {
				hub.CaptureCheckIn(&sentry.CheckIn{
					MonitorSlug: slug,
					Status:      sentry.CheckInStatusError,
				}, nil)
				hub.CaptureException(err)
			} else {
				hub.CaptureCheckIn(&sentry.CheckIn{
					MonitorSlug: slug,
					Status:      sentry.CheckInStatusOK,
				}, nil)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
				continue
			}
		}
	}()
}

func deleteOldPreviews(dir string, maxGb int) error {
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return err
	}

	var maxBytes int64 = int64(maxGb) * 1_000_000_000

	var dirSize int64
	for _, file := range files {
		dirSize += file.Size()
	}

	if dirSize <= maxBytes {
		return nil
	}

	sort.Slice(files, func(i, j int) bool { // Sort most recent first
		return files[i].ModTime().After(files[j].ModTime())
	})

	var runningDirSize int64 = 0
	var targetDirSize int64 = int64(float64(maxBytes) * 0.80)
	for _, file := range files {
		runningDirSize += file.Size()
		if runningDirSize > targetDirSize {
			if err = os.Remove(filepath.Join(dir, file.Name())); err != nil {
				return err
			}
		}
	}

	return nil
}

type mergeFS struct {
	a http.FileSystem
	b http.FileSystem
}

func (m *mergeFS) Open(name string) (http.File, error) {
	file, err := m.a.Open(name)
	if err == nil {
		return file, nil
	}

	file, err = m.b.Open(name)
	if err == nil {
		return file, nil
	}

	return nil, err
}
