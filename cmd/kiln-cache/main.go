// Command kiln-cache serves a remote build cache over the Bazel Remote
// Execution API v2 (CAS, ActionCache, Capabilities and ByteStream). Any REAPI
// client can use it, including Bazel, Buck2, reclient and kiln-cc.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/7shantanu7/kiln/internal/cacheserver"
	"github.com/7shantanu7/kiln/internal/cas"
)

func main() {
	dir := flag.String("dir", "", "storage directory")
	listen := flag.String("listen", "127.0.0.1:8980", "gRPC listen address")
	metrics := flag.String("metrics", "127.0.0.1:8981", "HTTP address for /metrics and /healthz (empty to disable)")
	maxGB := flag.Float64("max-size-gb", 10, "evict least recently used entries above this size (0 = unbounded)")
	flag.Parse()
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "kiln-cache: -dir is required")
		os.Exit(2)
	}

	store, err := cas.Open(*dir, int64(*maxGB*(1<<30)))
	if err != nil {
		log.Fatalf("kiln-cache: open store: %v", err)
	}
	entries, bytes, _ := store.Usage()
	srv := cacheserver.New(store)

	g := grpc.NewServer(grpc.MaxRecvMsgSize(cacheserver.MaxBatchBytes+(1<<20)), grpc.MaxSendMsgSize(cacheserver.MaxBatchBytes+(1<<20)))
	srv.Register(g)
	hs := health.NewServer()
	healthpb.RegisterHealthServer(g, hs)

	lis, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("kiln-cache: %v", err)
	}
	if *metrics != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; version=0.0.4")
			fmt.Fprint(w, srv.Metrics())
		})
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
		go func() {
			if err := http.ListenAndServe(*metrics, mux); err != nil {
				log.Printf("kiln-cache: metrics server: %v", err)
			}
		}()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		g.GracefulStop()
	}()
	log.Printf("kiln-cache: serving %s on %s (%d entries, %.1f MiB)", *dir, lis.Addr(), entries, float64(bytes)/(1<<20))
	if err := g.Serve(lis); err != nil {
		log.Fatalf("kiln-cache: %v", err)
	}
}
