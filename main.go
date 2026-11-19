package main

import (
	"embed"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"seedlink-auditor/internal/arr"
	"seedlink-auditor/internal/config"
	"seedlink-auditor/internal/qbit"
	"seedlink-auditor/internal/server"
)

//go:embed web/*
var embeddedWeb embed.FS

func main() {
	cfgMgr, err := config.NewManager("")
	if err != nil {
		log.Fatalf("failed to initialize configuration: %v", err)
	}

	webFS, err := fs.Sub(embeddedWeb, "web")
	if err != nil {
		log.Fatalf("failed to load embedded web assets: %v", err)
	}

	qbitClient := qbit.NewClient(20 * time.Second)
	arrClient := arr.NewClient(20 * time.Second)
	srv := server.New(cfgMgr, qbitClient, arrClient, webFS)

	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = "8090"
	}
	addr := ":" + port

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	log.Printf("SeedLink Auditor listening on http://0.0.0.0%s", addr)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}
