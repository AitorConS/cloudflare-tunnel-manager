// Command fakecf serves a simulated Cloudflare API for developing the UI
// without a real account: run it and start ctm with
// CTM_CF_API_BASE=http://127.0.0.1:8099.
package main

import (
	"log"
	"net/http"

	"tunnelmanager/internal/cftest"
)

func main() {
	log.Println("simulated Cloudflare API on http://127.0.0.1:8099")
	log.Fatal(http.ListenAndServe("127.0.0.1:8099", cftest.New()))
}
