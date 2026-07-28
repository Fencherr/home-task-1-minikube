package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	_ "github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	db           *sql.DB
	healthzTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "healthz_checks_total",
		Help: "Total number of health checks",
	})
	healthzErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "healthz_errors_total",
		Help: "Total number of failed health checks",
	})
	httpRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total number of HTTP requests",
	}, []string{"method", "path", "status"})
)

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL environment variable is required")
	}

	var err error
	db, err = sql.Open("postgres", dsn)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	http.HandleFunc("/", helloHandler)
	http.HandleFunc("/healthz", healthzHandler)
	http.Handle("/metrics", promhttp.Handler())

	addr := ":8080"
	log.Printf("starting server on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}

func helloHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		httpRequests.WithLabelValues(r.Method, r.URL.Path, "404").Inc()
		http.NotFound(w, r)
		return
	}
	httpRequests.WithLabelValues(r.Method, "/", "200").Inc()
	fmt.Fprintln(w, "Hello from QOVES API!")
}

func healthzHandler(w http.ResponseWriter, r *http.Request) {
	healthzTotal.Inc()
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		healthzErrors.Inc()
		httpRequests.WithLabelValues(r.Method, "/healthz", "503").Inc()
		log.Printf("health check failed: %v", err)
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		return
	}
	var result int
	if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&result); err != nil {
		healthzErrors.Inc()
		httpRequests.WithLabelValues(r.Method, "/healthz", "503").Inc()
		log.Printf("health check query failed: %v", err)
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		return
	}
	httpRequests.WithLabelValues(r.Method, "/healthz", "200").Inc()
	fmt.Fprintln(w, "OK")
}
