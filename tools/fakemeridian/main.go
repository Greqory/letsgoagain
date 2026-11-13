// Command fakemeridian serves internal/fakemeridian on a local port for sandbox runs against a real
// OpenCode. Switch it with POST /fake/limit {"limited":true,"retry_after":4868} and POST /fake/buckets.
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/Greqory/letsgoagain/internal/fakemeridian"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:47812", "listen address")
	flag.Parse()
	log.Printf("fakemeridian on http://%s", *addr)
	log.Fatal(http.ListenAndServe(*addr, &fakemeridian.Server{}))
}
