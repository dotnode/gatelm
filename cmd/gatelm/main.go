package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dotnode/gatelm/pkg/console"
	"github.com/dotnode/gatelm/pkg/gatelm"
)

func main() {
	debugFlag := flag.Bool("debug", false, "Enable debug logging")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [flags] [config.yaml]\n\nFlags:\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	cfgPath := "config.yaml"
	if flag.NArg() > 0 {
		cfgPath = flag.Arg(0)
	}

	// If config file does not exist, run interactive setup
	var cfg gatelm.Config
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		setupCfg, setupErr := runInteractiveSetup(cfgPath)
		if setupErr != nil {
			log.Fatalf("初始化引导失败: %v", setupErr)
		}
		cfg = *setupCfg
	} else {
		var loadErr error
		cfg, loadErr = gatelm.LoadConfig(cfgPath)
		if loadErr != nil {
			log.Fatalf("load config failed: %v", loadErr)
		}
	}

	gateway, err := gatelm.New(gatelm.Options{
		Config:     cfg,
		ConfigPath: cfgPath,
		Debug:      *debugFlag,
	})
	if err != nil {
		log.Fatalf("init gateway failed: %v", err)
	}
	defer gateway.Close()

	if *debugFlag || cfg.Debug {
		log.Printf("debug logging enabled, writing to logs/")
	}
	if cfg.MaxConcurrentRequests > 0 {
		log.Printf("concurrency limit: %d", cfg.MaxConcurrentRequests)
	}

	mux := http.NewServeMux()
	console.Mount(mux, gateway, console.Options{})
	mux.Handle("/", gateway.Handler())

	listen := cfg.Listen
	if listen == "" {
		listen = ":8080"
	}

	httpSrv := &http.Server{
		Addr:    listen,
		Handler: mux,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		log.Println("shutting down gracefully...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown error: %v", err)
		}
		if err := gateway.Shutdown(shutdownCtx); err != nil && err != context.Canceled && err != context.DeadlineExceeded {
			log.Printf("gateway shutdown error: %v", err)
		}
	}()

	log.Printf("proxy listen on %s", listen)
	if cfg.Console.Enabled {
		log.Printf("console: http://localhost%s/console", listen)
	}
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("listen failed: %v", err)
	}
	log.Println("server stopped")
}
