package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--healthcheck" {
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get("http://127.0.0.1:8080/healthz")
		if err != nil {
			os.Exit(1)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			os.Exit(1)
		}
		return
	}
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal("invalid configuration")
	}
	lock, err := lockState(cfg.StateDir)
	if err != nil {
		log.Fatal("state directory unavailable or already owned")
	}
	defer lock.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	svc, err := newService(cfg)
	if err != nil {
		log.Fatal("invalid persistent state")
	}
	go svc.run(ctx)
	server := &http.Server{Addr: cfg.Listen, Handler: svc, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	go func() {
		<-ctx.Done()
		stop, done := context.WithTimeout(context.Background(), 20*time.Second)
		defer done()
		_ = server.Shutdown(stop)
	}()
	log.Print("quota-api started")
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Print("HTTP listener failed")
		cancel()
	}
	<-svc.done
}
