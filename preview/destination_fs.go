package preview

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

var _ Destination = (*FSDestination)(nil)

type FSDestination struct {
	root   *os.Root
	server *http.Server
}

func NewFSDestination(ctx context.Context, config *url.URL) (*FSDestination, error) {
	path := config.Host + config.Path
	query := config.Query()
	dest := FSDestination{}
	var err error

	dest.root, err = os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("opening root: %w", err)
	}

	serverAddr := query.Get("server")
	if serverAddr != "" {
		err = dest.startServer(serverAddr)
		if err != nil {
			return nil, fmt.Errorf("starting server: %w", err)
		}
	}

	// rawCleanupGb := query.Get("cleanup")
	// if rawCleanupGb != "" {
	// 	cleanupGb, err := strconv.ParseFloat(rawCleanupGb, 32)
	// }

	// TODO start server if host and port, actually move to just using one "server" query
	// note: dont need to use merge fs, I will just "upload" the static files to the dest

	// TODO start cleanup job (remote sentry integration) if maxgb

	return &dest, nil
}

func (fs *FSDestination) startServer(addr string) error {
	fs.server = &http.Server{
		Addr:    addr,
		Handler: files(http.FileServer(http.Dir(fs.root.Name()))),

		ReadHeaderTimeout:            5 * time.Second,
		DisableGeneralOptionsHandler: true,
	}

	var startupErr error
	go func() {
		err := fs.server.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			startupErr = err
			//todo: log
		}
	}()

	time.Sleep(100 * time.Millisecond)
	return startupErr
}

func (fs *FSDestination) String() string {
	addr := "none"
	if fs.server != nil {
		addr = fs.server.Addr
	}
	return fmt.Sprintf("filesystem destination at %s server=%s", fs.root.Name(), addr)
}

func (fs *FSDestination) Close() error {
	var err error

	// Shutdown HTTP server if running
	if fs.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if shutdownErr := fs.server.Shutdown(ctx); shutdownErr != nil {
			err = shutdownErr
		}
	}

	// Close the root filesystem
	if closeErr := fs.root.Close(); closeErr != nil {
		if err != nil {
			return fmt.Errorf("multiple errors: server shutdown: %v, root close: %v", err, closeErr)
		}
		err = closeErr
	}

	return err
}

func (fs *FSDestination) Upload(ctx context.Context, name string, content []byte) error {
	if err := validateSimpleFilename(name); err != nil {
		return err
	}

	return fs.root.WriteFile(name, content, os.FileMode(0644))
}

func (fs *FSDestination) Download(ctx context.Context, name string) ([]byte, error) {
	if err := validateSimpleFilename(name); err != nil {
		return nil, err
	}

	return fs.root.ReadFile(name) // TODO normalized not found
}

func files(next http.Handler) http.Handler {
	return http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
		if req.Method != "GET" && req.Method != "HEAD" {
			res.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		// prevent directory listings
		if req.URL.Path != "/" && strings.HasSuffix(req.URL.Path, "/") {
			http.NotFound(res, req)
			return
		}

		// TODO revisit this
		if strings.HasSuffix(req.URL.Path, ".mp4") {
			res.Header().Set("Content-Type", "video/mp4")
			res.Header().Set("Cache-Control", "public, max-age=31536000")
		} else {
			res.Header().Set("Cache-Control", "public, max-age=3600")
		}

		next.ServeHTTP(res, req)
	})
}

//
////go:embed web/*
//var webFS embed.FS
//
//func oldmain(ctx context.Context) {
//
//	job(ctx, "delete-old-previews", time.Minute, time.Hour, func() error {
//		return deleteOldPreviews(args.StaticDir, 20)
//	})
//
//	subFS, err := fs.Sub(webFS, "web")
//	if err != nil {
//		return fmt.Errorf("fs.Sub: %w", err)
//	}
//
//	httpServer := &http.Server{
//		Addr:              args.Host + ":" + args.Port,
//		Handler:           files(http.FileServer(&mergeFS{http.FS(subFS), http.Dir(args.StaticDir)})),
//		ReadHeaderTimeout: 10 * time.Second,
//	}
//
//	errChan := make(chan error, 1)
//	go func() {
//		slog.InfoContext(ctx, "httpServer started", "addr", httpServer.Addr)
//		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
//			errChan <- err
//		}
//	}()
//
//	http.FS(webFS)
//
//	select {
//	case err := <-errChan:
//		return err
//	case <-ctx.Done():
//		slog.InfoContext(ctx, "shutting down httpServer")
//	}
//
//	ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
//	defer cancel()
//	return httpServer.Shutdown(ctx)
//}
//
//func files(next http.Handler) http.Handler {
//	return http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
//		if req.Method != "GET" && req.Method != "HEAD" {
//			res.WriteHeader(http.StatusMethodNotAllowed)
//			return
//		}
//
//		// Prevent directory listings
//		if req.URL.Path != "/" && strings.HasSuffix(req.URL.Path, "/") {
//			http.NotFound(res, req)
//			return
//		}
//
//		if strings.HasSuffix(req.URL.Path, ".mp4") {
//			res.Header().Set("Content-Type", "video/mp4")
//			res.Header().Set("Cache-Control", "public, max-age=31536000")
//		} else {
//			res.Header().Set("Cache-Control", "public, max-age=3600")
//		}
//
//		next.ServeHTTP(res, req)
//	})
//}
//
//func job(ctx context.Context, slug string, timeout, interval time.Duration, jobFunc func() error) {
//	go func() {
//		hub := sentry.CurrentHub().Clone()
//		hub.ConfigureScope(func(scope *sentry.Scope) {
//			scope.SetContext("monitor", sentry.Context{"slug": slug})
//		})
//		time.Sleep(timeout)
//		for {
//			hub.CaptureCheckIn(&sentry.CheckIn{
//				MonitorSlug: slug,
//				Status:      sentry.CheckInStatusInProgress,
//			}, nil)
//			if err := jobFunc(); err != nil {
//				hub.CaptureCheckIn(&sentry.CheckIn{
//					MonitorSlug: slug,
//					Status:      sentry.CheckInStatusError,
//				}, nil)
//				hub.CaptureException(err)
//			} else {
//				hub.CaptureCheckIn(&sentry.CheckIn{
//					MonitorSlug: slug,
//					Status:      sentry.CheckInStatusOK,
//				}, nil)
//			}
//			select {
//			case <-ctx.Done():
//				return
//			case <-time.After(interval):
//				continue
//			}
//		}
//	}()
//}
//
//func deleteOldPreviews(dir string, maxGb int) error {
//	files, err := ioutil.ReadDir(dir)
//	if err != nil {
//		return err
//	}
//
//	var maxBytes int64 = int64(maxGb) * 1_000_000_000
//
//	var dirSize int64
//	for _, file := range files {
//		dirSize += file.Size()
//	}
//
//	if dirSize <= maxBytes {
//		return nil
//	}
//
//	sort.Slice(files, func(i, j int) bool { // Sort most recent first
//		return files[i].ModTime().After(files[j].ModTime())
//	})
//
//	var runningDirSize int64 = 0
//	var targetDirSize int64 = int64(float64(maxBytes) * 0.80)
//	for _, file := range files {
//		runningDirSize += file.Size()
//		if runningDirSize > targetDirSize {
//			if err = os.Remove(filepath.Join(dir, file.Name())); err != nil {
//				return err
//			}
//		}
//	}
//
//	return nil
//}
//
//type mergeFS struct {
//	a http.FileSystem
//	b http.FileSystem
//}
//
//func (m *mergeFS) Open(name string) (http.File, error) {
//	file, err := m.a.Open(name)
//	if err == nil {
//		return file, nil
//	}
//
//	file, err = m.b.Open(name)
//	if err == nil {
//		return file, nil
//	}
//
//	return nil, err
//}
