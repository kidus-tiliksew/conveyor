// conveyord is the control-plane daemon: orchestrator, task queue, and
// HTTP API (spec §3.1). Phase 1 runs with an in-memory store; Phase 2
// swaps in Postgres (event-sourced) + River without changing this
// binary's shape. The Phase 2+ dashboard SPA embeds here via go:embed
// so the whole control plane ships as one binary (spec §17.0).
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	srv := httpapi.NewServer(store.NewMemory())

	log.Printf("conveyord listening on %s", *addr)
	if err := http.ListenAndServe(*addr, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}
