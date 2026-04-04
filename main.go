package main

import (
	"log"
	"net/http"
	"os"
	"strconv"
)

func main() {
	token := mustEnv("GITHUB_TOKEN")
	secret := mustEnv("WEBHOOK_SECRET")
	org := envOrDefault("GITHUB_ORG", "derio-net")
	projectNum := envOrDefaultInt("PROJECT_NUMBER", 1)
	port := envOrDefault("PORT", "8080")

	bridge, err := NewBridge(token, org, projectNum)
	if err != nil {
		log.Fatalf("Failed to initialize bridge: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook", bridge.WebhookHandler(secret))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if bridge.Ready() {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ready"))
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("not ready"))
		}
	})

	log.Printf("health-bridge listening on :%s (org=%s, project=%d)", port, org, projectNum)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

func mustEnv(key string) string {
	val := os.Getenv(key)
	if val == "" {
		log.Fatalf("Required environment variable %s is not set", key)
	}
	return val
}

func envOrDefault(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}

func envOrDefaultInt(key string, fallback int) int {
	if val := os.Getenv(key); val != "" {
		n, err := strconv.Atoi(val)
		if err != nil {
			log.Fatalf("Environment variable %s must be an integer, got %q", key, val)
		}
		return n
	}
	return fallback
}
