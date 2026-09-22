// wlb-agent: процесс №1 в поде обхода. Один под = один человек.
//
// Держит по headless-creator'у на каждую платформу, в которую человек залогинился
// через GUI-Creator (появился cookies-<платформа>.json). Упал creator — поднимает
// заново, а он создаёт новую комнату. Ссылки отдаёт боту по HTTP внутри кластера.
//
// Пока идёт вход через wlb-login, новые creator'ы не стартуют: GUI в это время
// сам пишет куки и сам держит свой creator.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type platform struct {
	key    string // короткое имя: vk, tm, wb, dion
	bin    string
	cookie string // имя файла кук в userData GUI-Creator'а
	extra  []string
}

// порядок = порядок вывода
var platforms = []platform{
	{key: "wb", bin: "headless-wbstream-creator", cookie: "cookies-wbstream.json", extra: []string{"--name", "Home"}},
	{key: "vk", bin: "headless-vk-creator", cookie: "cookies-vk.json"},
	{key: "tm", bin: "headless-telemost-creator", cookie: "cookies-telemost.json"},
	{key: "dion", bin: "headless-dion-creator", cookie: "cookies-dion.json", extra: []string{"--name", "Home"}},
}

const (
	recreateWait = 90 * time.Second // сколько ждём свежую ссылку после пересоздания
	minBackoff   = 5 * time.Second
	maxBackoff   = 2 * time.Minute
	stableRun    = 2 * time.Minute // проработал дольше — backoff сбрасывается
)

type config struct {
	binsDir, cookiesDir, roomsDir, loginFlag string
	upstreamSocks, resources                 string
}

// runner держит один creator одной платформы.
type runner struct {
	p   platform
	cfg *config

	mu       sync.Mutex
	cmd      *exec.Cmd
	since    time.Time
	recreate bool // текущий процесс убит по /recreate — перезапуск без backoff
}

func (r *runner) roomFile() string     { return filepath.Join(r.cfg.roomsDir, r.p.key+".txt") }
func (r *runner) notifiedFile() string { return filepath.Join(r.cfg.roomsDir, r.p.key+".notified") }
func (r *runner) cookiePath() string   { return filepath.Join(r.cfg.cookiesDir, r.p.cookie) }

// loggedIn: файл кук есть и это непустой JSON. Недописанный файл во время
// логина читается как «ещё нет».
func (r *runner) loggedIn() bool {
	data, err := os.ReadFile(r.cookiePath())
	if err != nil || len(strings.TrimSpace(string(data))) < 3 {
		return false
	}
	return json.Valid(data)
}

func loginActive(cfg *config) bool {
	_, err := os.Stat(cfg.loginFlag)
	return err == nil
}

func (r *runner) running() (bool, time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cmd != nil, r.since
}

func (r *runner) loop(ctx context.Context) {
	backoff := minBackoff
	for ctx.Err() == nil {
		if !r.loggedIn() || loginActive(r.cfg) {
			sleep(ctx, 5*time.Second)
			continue
		}
		started := time.Now()
		err := r.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		r.mu.Lock()
		forced := r.recreate
		r.recreate = false
		r.mu.Unlock()
		if forced {
			log.Printf("[%s] пересоздание по запросу", r.p.key)
			backoff = minBackoff
			continue
		}
		if time.Since(started) > stableRun {
			backoff = minBackoff
		}
		log.Printf("[%s] creator завершился: %v; перезапуск через %s", r.p.key, err, backoff)
		sleep(ctx, backoff)
		backoff = min(backoff*2, maxBackoff)
	}
}

func (r *runner) runOnce(ctx context.Context) error {
	// старая ссылка после рестарта мертва — не отдаём её как текущую
	if err := os.WriteFile(r.roomFile(), nil, 0o644); err != nil {
		return err
	}
	args := []string{
		"--cookies", r.cookiePath(),
		"--write-file", r.roomFile(),
		"--resources", r.cfg.resources,
	}
	if r.cfg.upstreamSocks != "" {
		args = append(args, "--upstream-socks", r.cfg.upstreamSocks)
	}
	args = append(args, r.p.extra...)

	cmd := exec.Command(filepath.Join(r.cfg.binsDir, r.p.bin), args...)
	cmd.Stdout = prefixWriter(r.p.key)
	cmd.Stderr = cmd.Stdout
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	r.mu.Lock()
	r.cmd, r.since = cmd, time.Now()
	r.mu.Unlock()
	log.Printf("[%s] creator запущен, pid=%d", r.p.key, cmd.Process.Pid)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		killGroup(cmd, syscall.SIGTERM)
		select {
		case err = <-done:
		case <-time.After(10 * time.Second):
			killGroup(cmd, syscall.SIGKILL)
			err = <-done
		}
	}
	r.mu.Lock()
	r.cmd = nil
	r.mu.Unlock()
	return err
}

// kill останавливает текущий creator; loop поднимет новый сразу, без backoff.
func (r *runner) kill() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cmd == nil {
		return false
	}
	r.recreate = true
	killGroup(r.cmd, syscall.SIGTERM)
	return true
}

func killGroup(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, sig)
	}
}

// readLast возвращает последнюю непустую строку файла: creator'ы дописывают ссылку.
func readLast(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			return s
		}
	}
	return ""
}

type room struct {
	Platform string `json:"platform"`
	LoggedIn bool   `json:"logged_in"`
	Running  bool   `json:"running"`
	Link     string `json:"link,omitempty"`
	Notified bool   `json:"notified"` // эту ссылку бот уже отправил владельцу
}

func (r *runner) state() room {
	running, _ := r.running()
	link := readLast(r.roomFile())
	return room{
		Platform: r.p.key,
		LoggedIn: r.loggedIn(),
		Running:  running,
		Link:     link,
		Notified: link != "" && readLast(r.notifiedFile()) == link,
	}
}

type server struct {
	cfg     *config
	runners map[string]*runner
	order   []string
}

func (s *server) handleRooms(w http.ResponseWriter, _ *http.Request) {
	out := struct {
		Login bool   `json:"login"`
		Rooms []room `json:"rooms"`
	}{Login: loginActive(s.cfg)}
	for _, k := range s.order {
		out.Rooms = append(out.Rooms, s.runners[k].state())
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleRecreate(w http.ResponseWriter, req *http.Request) {
	r, ok := s.runners[req.PathValue("platform")]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown platform"})
		return
	}
	if !r.loggedIn() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "not logged in"})
		return
	}
	_ = os.WriteFile(r.roomFile(), nil, 0o644)
	if !r.kill() {
		// процесса нет (backoff после падения) — ждём, пока loop поднимет сам
		log.Printf("[%s] recreate: creator не запущен, жду старта", r.p.key)
	}
	deadline := time.Now().Add(recreateWait)
	for time.Now().Before(deadline) {
		if link := readLast(r.roomFile()); link != "" {
			writeJSON(w, http.StatusOK, r.state())
			return
		}
		select {
		case <-req.Context().Done():
			return
		case <-time.After(time.Second):
		}
	}
	writeJSON(w, http.StatusGatewayTimeout, map[string]string{"error": "no link within " + recreateWait.String()})
}

// handleNotified: бот отметил, что эту ссылку владелец получил. Отметка живёт на
// PVC, поэтому рестарт бота или пода не шлёт ту же ссылку повторно.
func (s *server) handleNotified(w http.ResponseWriter, req *http.Request) {
	r, ok := s.runners[req.PathValue("platform")]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown platform"})
		return
	}
	var body struct {
		Link string `json:"link"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.Link == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "link required"})
		return
	}
	if err := os.WriteFile(r.notifiedFile(), []byte(body.Link+"\n"), 0o644); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, r.state())
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

type lineWriter struct {
	prefix string
	mu     sync.Mutex
	buf    []byte
}

func prefixWriter(p string) *lineWriter { return &lineWriter{prefix: "[" + p + "] "} }

func (l *lineWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, b...)
	for {
		i := strings.IndexByte(string(l.buf), '\n')
		if i < 0 {
			break
		}
		os.Stdout.Write(append([]byte(l.prefix), l.buf[:i+1]...))
		l.buf = l.buf[i+1:]
	}
	return len(b), nil
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	log.SetFlags(log.LstdFlags)
	home := env("HOME", "/data")
	cfg := &config{}
	listen := flag.String("listen", ":8080", "адрес HTTP для бота")
	flag.StringVar(&cfg.binsDir, "bins-dir", "/opt/wlb/bin", "каталог headless-*-creator")
	flag.StringVar(&cfg.cookiesDir, "cookies-dir", filepath.Join(home, ".config/whitelist-bypass-creator"), "userData GUI-Creator'а, где лежат cookies-*.json")
	flag.StringVar(&cfg.roomsDir, "rooms-dir", filepath.Join(home, "rooms"), "каталог room-файлов")
	flag.StringVar(&cfg.loginFlag, "login-flag", "/tmp/wlb-login/active", "файл-флаг: идёт вход через wlb-login")
	flag.StringVar(&cfg.upstreamSocks, "upstream-socks", os.Getenv("UPSTREAM_SOCKS"), "SOCKS5 выхода для трафика туннеля")
	flag.StringVar(&cfg.resources, "resources", env("RESOURCES", "default"), "режим ресурсов creator'ов")
	flag.Parse()

	for _, d := range []string{cfg.cookiesDir, cfg.roomsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			log.Fatalf("mkdir %s: %v", d, err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	s := &server{cfg: cfg, runners: map[string]*runner{}}
	var wg sync.WaitGroup
	for _, p := range platforms {
		r := &runner{p: p, cfg: cfg}
		s.runners[p.key] = r
		s.order = append(s.order, p.key)
		wg.Add(1)
		go func() { defer wg.Done(); r.loop(ctx) }()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /rooms", s.handleRooms)
	mux.HandleFunc("POST /recreate/{platform}", s.handleRecreate)
	mux.HandleFunc("POST /notified/{platform}", s.handleNotified)
	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http: %v", err)
		}
	}()
	log.Printf("wlb-agent: listen=%s cookies=%s upstream=%q", *listen, cfg.cookiesDir, cfg.upstreamSocks)

	<-ctx.Done()
	log.Printf("wlb-agent: остановка")
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	wg.Wait()
	fmt.Println("wlb-agent: все creator'ы остановлены")
}
