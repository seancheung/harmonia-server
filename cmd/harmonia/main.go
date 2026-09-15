package main

import (
	"context"
	"github.com/harmonia/harmonia-server/internal/app"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	cfg := app.ConfigFromEnv()
	a, e := app.New(cfg)
	if e != nil {
		log.Fatal(e)
	}
	defer a.Close()
	server := &http.Server{Addr: cfg.Listen, Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = server.Shutdown(c)
	}()
	log.Printf("Harmonia listening on %s", cfg.Listen)
	if e = server.ListenAndServe(); e != nil && e != http.ErrServerClosed {
		log.Fatal(e)
	}
}
