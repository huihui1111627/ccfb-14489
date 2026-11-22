// Command gateway serves the HTTP/JSON <-> binary transcoding gateway and the
// inspector UI.
//
//	go run ./cmd/gateway -schema ./testdata -addr :8080
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"example.com/idlbridge/internal/backend"
	"example.com/idlbridge/internal/gateway"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	schemaDir := flag.String("schema", "./testdata", "directory of .proto files")
	webDir := flag.String("web", "./web", "directory containing the inspector UI")
	verbose := flag.Bool("v", true, "log each request")
	flag.Parse()

	schema, err := gateway.LoadDir(*schemaDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load schema:", err)
		os.Exit(1)
	}
	log.Printf("loaded %d files, %d methods, %d routes",
		len(schema.Files), len(schema.Methods), len(schema.Bindings))

	fake := backend.New(schema.Registry)
	fake.Now = time.Now

	var logger *log.Logger
	if *verbose {
		logger = log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds)
	}
	gw, err := gateway.New(gateway.Options{
		Schema:          schema,
		Backend:         fake.Router(),
		Logger:          logger,
		ExposeInspector: true,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "gateway:", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	if _, err := os.Stat(*webDir); err == nil {
		mux.Handle("/", http.FileServer(http.Dir(*webDir)))
	} else {
		log.Printf("web directory %q not found; only API endpoints are served", *webDir)
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "inspector UI not installed; run from the repository root or pass -web", http.StatusNotFound)
		})
	}
	mux.Handle("/v1/", gw)
	mux.Handle("/_inspect", gw)
	mux.Handle("/_schema", gw)
	mux.Handle("/_bench", gw)
	mux.Handle("/healthz", gw)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           withCORS(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("listening on http://localhost%s", *addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}
}

func withCORS(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Accept, Grpc-Timeout")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Expose-Headers", "X-Status-Code, Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}
