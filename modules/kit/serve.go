package kit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/stephenmuema/finnie/pkg/provapi"
)

// Serve runs a provider module: the provider protocol on POST / (signed with
// ZIRNIS_MODULE_SECRET, the same secret as the endpoint's secretEnv in Zirnis) and a health
// probe on GET /healthz, on MODULE_ADDR (default :8090). It returns when the process is told to
// stop. The module is never exposed to the internet: providers' callbacks reach it through
// Zirnis's relay.
//
// "<binary> healthcheck" probes a running module instead (for container health checks).
func Serve(name string, m provapi.Module) error {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		return healthcheck()
	}
	secret := os.Getenv("ZIRNIS_MODULE_SECRET")
	if len(secret) < 32 {
		return errors.New("ZIRNIS_MODULE_SECRET must be set to at least 32 random characters (the same value as the module endpoint's secret in Zirnis)")
	}
	addr := Env("MODULE_ADDR", ":8090")
	mux := http.NewServeMux()
	mux.Handle("POST /", provapi.Handler(m, secret))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"status":"ok","module":%q,"providers":%q}`, name, strings.Join(m.Names(), ","))
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errs := make(chan error, 1)
	go func() { errs <- srv.ListenAndServe() }()
	slog.Info("provider module listening", "module", name, "addr", addr, "providers", m.Names())
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

func healthcheck() error {
	addr := Env("MODULE_ADDR", ":8090")
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check: HTTP %d", resp.StatusCode)
	}
	os.Exit(0)
	return nil
}

// Env returns an environment variable, or fallback when it is unset or empty.
func Env(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

// Required collects missing required variables, so a module reports them all at once.
type Required []string

// Get returns a required variable, noting it when missing.
func (r *Required) Get(name string) string {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		*r = append(*r, name)
	}
	return v
}

// Err reports the missing variables, if any.
func (r Required) Err() error {
	if len(r) == 0 {
		return nil
	}
	return fmt.Errorf("missing required configuration: %s", strings.Join(r, ", "))
}

// Fatal logs err and exits.
func Fatal(err error) {
	slog.Error("provider module stopped", "error", err)
	os.Exit(1)
}
