package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"nexcall/server/internal/api"
	"nexcall/server/internal/auth"
	sig "nexcall/server/internal/signal"
	"nexcall/server/internal/store"
)

type config struct {
	listen      string
	dbPath      string
	livekit     auth.LiveKit
	session     string
	origins     []string
	webDir      string
	ringTimeout time.Duration
	logLevel    string
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func loadConfig() config {
	ring, err := time.ParseDuration(env("RING_TIMEOUT", "45s"))
	if err != nil {
		ring = 45 * time.Second
	}
	var origins []string
	if o := env("ALLOWED_ORIGINS", ""); o != "" {
		origins = strings.Split(o, ",")
	}
	return config{
		listen: env("LISTEN_ADDR", ":8080"),
		dbPath: env("DB_PATH", "nexcall.db"),
		livekit: auth.LiveKit{
			URL:       env("LIVEKIT_URL", "ws://localhost:7880"),
			APIKey:    env("LIVEKIT_API_KEY", ""),
			APISecret: env("LIVEKIT_API_SECRET", ""),
		},
		session:     env("SESSION_SECRET", ""),
		origins:     origins,
		webDir:      env("WEB_DIR", ""),
		ringTimeout: ring,
		logLevel:    env("LOG_LEVEL", "info"),
	}
}

const usage = `call-server - 实时通话业务服务

用法:
  call-server [serve]                       启动服务
  call-server user add <用户名> [密码]       添加/重置 Web 用户（不给密码则交互输入）
  call-server user del <用户名>
  call-server user list
  call-server device add <设备ID> [名称]     登记设备并生成密钥（重复执行会重置密钥）
  call-server device del <设备ID>
  call-server device list
  call-server token <房间> <身份>            签发调试用 LiveKit Token（直连模式）

环境变量: LISTEN_ADDR DB_PATH LIVEKIT_URL LIVEKIT_API_KEY LIVEKIT_API_SECRET
          SESSION_SECRET ALLOWED_ORIGINS WEB_DIR RING_TIMEOUT LOG_LEVEL
`

func main() {
	cfg := loadConfig()
	var level slog.Level
	_ = level.UnmarshalText([]byte(cfg.logLevel))
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	args := os.Args[1:]
	if len(args) == 0 {
		args = []string{"serve"}
	}
	var err error
	switch args[0] {
	case "serve":
		err = serve(cfg)
	case "user":
		err = withStore(cfg, func(st *store.Store) error { return userCmd(st, args[1:]) })
	case "device":
		err = withStore(cfg, func(st *store.Store) error { return deviceCmd(st, args[1:]) })
	case "token":
		err = tokenCmd(cfg, args[1:])
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func withStore(cfg config, fn func(*store.Store) error) error {
	st, err := store.Open(cfg.dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	return fn(st)
}

func requireSecrets(cfg config) error {
	var missing []string
	if cfg.livekit.APIKey == "" {
		missing = append(missing, "LIVEKIT_API_KEY")
	}
	if cfg.livekit.APISecret == "" {
		missing = append(missing, "LIVEKIT_API_SECRET")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing env: %s", strings.Join(missing, ", "))
	}
	return nil
}

type directory struct{ st *store.Store }

func (d directory) Devices(ctx context.Context) ([]sig.DeviceStatus, error) {
	list, err := d.st.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]sig.DeviceStatus, 0, len(list))
	for _, dev := range list {
		out = append(out, sig.DeviceStatus{ID: dev.ID, Name: dev.Name, LastSeen: dev.LastSeen})
	}
	return out, nil
}

func (d directory) Touch(ctx context.Context, id string, t time.Time) {
	if err := d.st.TouchDevice(ctx, id, t); err != nil {
		slog.Warn("touch device", "err", err)
	}
}

func serve(cfg config) error {
	if err := requireSecrets(cfg); err != nil {
		return err
	}
	if len(cfg.session) < 16 {
		return errors.New("SESSION_SECRET must be at least 16 characters")
	}
	st, err := store.Open(cfg.dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.MarkDanglingCalls(context.Background(), time.Now()); err != nil {
		slog.Warn("mark dangling calls", "err", err)
	}

	hub := sig.NewHub(&cfg.livekit, st, directory{st}, cfg.livekit.URL, cfg.ringTimeout)
	srv := &api.Server{
		Store:          st,
		Hub:            hub,
		Sessions:       auth.NewSessions(cfg.session, 7*24*time.Hour),
		LiveKitURL:     cfg.livekit.URL,
		OriginPatterns: cfg.origins,
		WebDir:         cfg.webDir,
	}
	httpSrv := &http.Server{
		Addr:              cfg.listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	slog.Info("call-server listening", "addr", cfg.listen, "livekit", cfg.livekit.URL)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func readPassword() (string, error) {
	fmt.Fprint(os.Stderr, "密码: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func userCmd(st *store.Store, args []string) error {
	ctx := context.Background()
	if len(args) == 0 {
		return errors.New("user add|del|list")
	}
	switch args[0] {
	case "add":
		if len(args) < 2 {
			return errors.New("user add <username> [password]")
		}
		pw := ""
		if len(args) > 2 {
			pw = args[2]
		} else {
			var err error
			if pw, err = readPassword(); err != nil {
				return err
			}
		}
		if len(pw) < 8 {
			return errors.New("password must be at least 8 characters")
		}
		hash, err := auth.HashPassword(pw)
		if err != nil {
			return err
		}
		if err := st.UpsertUser(ctx, args[1], hash); err != nil {
			return err
		}
		fmt.Println("用户已保存:", args[1])
	case "del":
		if len(args) < 2 {
			return errors.New("user del <username>")
		}
		return st.DeleteUser(ctx, args[1])
	case "list":
		users, err := st.ListUsers(ctx)
		if err != nil {
			return err
		}
		for _, u := range users {
			fmt.Println(u)
		}
	default:
		return errors.New("user add|del|list")
	}
	return nil
}

func deviceCmd(st *store.Store, args []string) error {
	ctx := context.Background()
	if len(args) == 0 {
		return errors.New("device add|del|list")
	}
	switch args[0] {
	case "add":
		if len(args) < 2 {
			return errors.New("device add <id> [name]")
		}
		id := args[1]
		if strings.ContainsAny(id, ": ") {
			return errors.New("device id must not contain ':' or spaces")
		}
		name := id
		if len(args) > 2 {
			name = strings.Join(args[2:], " ")
		}
		secret, hash, err := auth.NewDeviceSecret()
		if err != nil {
			return err
		}
		if err := st.UpsertDevice(ctx, id, name, hash); err != nil {
			return err
		}
		fmt.Printf("设备已登记，请把以下配置写入香橙派 /etc/nexcall/device.env（密钥只显示这一次）:\n\nDEVICE_ID=%s\nDEVICE_SECRET=%s\n", id, secret)
	case "del":
		if len(args) < 2 {
			return errors.New("device del <id>")
		}
		return st.DeleteDevice(ctx, args[1])
	case "list":
		devices, err := st.ListDevices(ctx)
		if err != nil {
			return err
		}
		for _, d := range devices {
			seen := "-"
			if d.LastSeen != nil {
				seen = d.LastSeen.Format(time.DateTime)
			}
			fmt.Printf("%-16s %-20s last_seen=%s\n", d.ID, d.Name, seen)
		}
	default:
		return errors.New("device add|del|list")
	}
	return nil
}

func tokenCmd(cfg config, args []string) error {
	fs := flag.NewFlagSet("token", flag.ContinueOnError)
	ttl := fs.Duration("ttl", time.Hour, "有效期")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return errors.New("token [-ttl 1h] <room> <identity>")
	}
	if err := requireSecrets(cfg); err != nil {
		return err
	}
	lk := cfg.livekit
	lk.TTL = *ttl
	t, err := lk.RoomToken(fs.Arg(0), fs.Arg(1), fs.Arg(1))
	if err != nil {
		return err
	}
	fmt.Println(t)
	return nil
}
