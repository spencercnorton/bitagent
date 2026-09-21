package ui

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/spencercnorton/bitagent/internal/httpserver"
	"github.com/spencercnorton/bitagent/internal/worker"
	"go.uber.org/fx"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zapio"
)

const (
	workerKey   = "ui"
	maxBackoff  = 60 * time.Second
	stopTimeout = 10 * time.Second
)

type Params struct {
	fx.In
	Config     Config
	HTTPServer httpserver.Config
	Logger     *zap.Logger
}

type Result struct {
	fx.Out
	Worker worker.Worker `group:"workers"`
	// AppHook: on SIGTERM fx.Run only runs lifecycle hooks, never the worker
	// registry's Stop (the CLI context is context.Background()), so the
	// graceful SIGTERM to uvicorn has to hang off the lifecycle.
	AppHook fx.Hook `group:"app_hooks"`
}

func New(p Params) Result {
	w := newUIWorker(p.Config, coreURL(p.HTTPServer.LocalAddress), p.Logger)
	return Result{
		Worker:  worker.NewWorker(workerKey, fx.Hook{OnStart: w.start, OnStop: w.stop}),
		AppHook: fx.Hook{OnStop: w.stop},
	}
}

type uiWorker struct {
	cfg     Config
	coreURL string
	logger  *zap.Logger

	// ctx is created up front so stop() from the fx lifecycle can race (or
	// precede) start() from the worker registry without a data race or an
	// orphaned child: a start after stop spawns nothing.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func newUIWorker(cfg Config, coreURL string, logger *zap.Logger) *uiWorker {
	ctx, cancel := context.WithCancel(context.Background())
	return &uiWorker{cfg: cfg, coreURL: coreURL, logger: logger.Named(workerKey), ctx: ctx, cancel: cancel}
}

// coreURL maps the http_server listen address to what the child should dial:
// a wildcard/empty host means "this container", i.e. loopback.
func coreURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://127.0.0.1:3333"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func (w *uiWorker) start(context.Context) error {
	if !w.cfg.Enabled {
		w.logger.Info("ui disabled via config; skipping start")
		return nil
	}
	if _, _, err := net.SplitHostPort(w.cfg.ListenAddress); err != nil {
		return errors.New("ui: listen_address must be host:port: " + err.Error())
	}
	if w.cfg.RestartBackoff <= 0 {
		return errors.New("ui: restart_backoff must be positive when enabled")
	}
	w.wg.Add(1)
	go w.supervise(w.ctx)
	w.logger.Info("ui started", zap.String("listen", w.cfg.ListenAddress), zap.String("dir", w.cfg.Dir))
	return nil
}

func (w *uiWorker) stop(context.Context) error {
	w.cancel()
	w.wg.Wait()
	return nil
}

func (w *uiWorker) supervise(ctx context.Context) {
	defer w.wg.Done()
	backoff := w.cfg.RestartBackoff
	for {
		started := time.Now()
		err := w.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		// A child that stayed up past the cap was healthy: start the ladder over.
		if time.Since(started) > maxBackoff {
			backoff = w.cfg.RestartBackoff
		}
		w.logger.Warn("ui exited; restarting", zap.Error(err), zap.Duration("backoff", backoff))
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func (w *uiWorker) runOnce(ctx context.Context) error {
	host, port, _ := net.SplitHostPort(w.cfg.ListenAddress)
	// --no-proxy-headers is load-bearing: the UI's proxy-tier auth trusts the
	// transport peer address (see ui/auth.py), which X-Forwarded-For must not replace.
	cmd := exec.CommandContext(ctx, w.cfg.Python, "-m", "uvicorn", "app:app", "--no-proxy-headers", "--host", host, "--port", port)
	cmd.Dir = w.cfg.Dir
	cmd.Env = os.Environ()
	for _, kv := range [][2]string{{"BITAGENT_GRAPHQL_URL", w.coreURL + "/graphql"}, {"BITAGENT_METRICS_URL", w.coreURL + "/metrics"}} {
		if _, set := os.LookupEnv(kv[0]); !set {
			cmd.Env = append(cmd.Env, kv[0]+"="+kv[1])
		}
	}
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = stopTimeout
	// The image runs the core as root (the composes mount /root/...), but the
	// web-facing child gets no more than the old bitagent-ui image had: appuser.
	if os.Geteuid() == 0 {
		if u, err := user.Lookup("appuser"); err == nil {
			uid, _ := strconv.Atoi(u.Uid)
			gid, _ := strconv.Atoi(u.Gid)
			cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}}
			cmd.Env = append(cmd.Env, "HOME="+u.HomeDir)
		} else {
			w.logger.Warn("ui: appuser not found; running child as root", zap.Error(err))
		}
	}
	// uvicorn's own logger (startup, errors, tracebacks) and the app's logging
	// write INFO lines to stderr; the access log is stdout. Both land at Info;
	// "stream" tells them apart. No caller: it would name zapio, not the child.
	child := w.logger.WithOptions(zap.WithCaller(false))
	stdout := &zapio.Writer{Log: child.With(zap.String("stream", "stdout")), Level: zapcore.InfoLevel}
	stderr := &zapio.Writer{Log: child.With(zap.String("stream", "stderr")), Level: zapcore.InfoLevel}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	stdout.Close()
	stderr.Close()
	return err
}
