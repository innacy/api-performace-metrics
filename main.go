package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"apiperf/orch"
	"apiperf/server"
	"apiperf/store"
)

func main() {
	raiseFDLimit()

	// Acquire the port first: it's the process mutex for "am I the only
	// apiperf running." Binding must happen before we touch the store —
	// otherwise a second instance started while a first is mid-run can
	// run the stale-run sweep and mark the first instance's live run
	// 'killed' before discovering the port is already taken.
	ln, err := net.Listen("tcp", "localhost:7071")
	if err != nil {
		log.Fatalf("apiperf: cannot bind localhost:7071 (already running?): %v", err)
	}

	st, err := store.Open("apiperf.db")
	if err != nil {
		ln.Close()
		log.Fatal(err)
	}
	if n, err := st.MarkStaleRunsKilled(); err != nil {
		log.Printf("main: MarkStaleRunsKilled: %v", err)
	} else if n > 0 {
		log.Printf("main: marked %d stale running run(s) as killed", n)
	}

	hub := server.NewHub()
	o := orch.New(st, hub.Emit)
	srv := &http.Server{Handler: server.New(st, o, hub)}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("apiperf: shutting down...")
		if running, id := o.Running(); running {
			o.Stop(id)
			// Give the run goroutine time to flush its buffered samples and
			// mark the run 'killed' before we close the store.
			for i := 0; i < 100; i++ {
				if running, _ := o.Running(); !running {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("main: server shutdown: %v", err)
		}
	}()

	log.Println("apiperf listening on http://localhost:7071")
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("main: serve: %v", err)
	}
	if err := st.Close(); err != nil {
		log.Printf("main: store close: %v", err)
	}
}

// raiseFDLimit lifts the soft open-files limit to the hard limit so
// high-concurrency stages don't die on EMFILE.
func raiseFDLimit() {
	var l syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &l); err == nil {
		l.Cur = l.Max
		_ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &l)
	}
}
