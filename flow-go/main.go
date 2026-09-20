// Command flow-go is a Google Flow generation engine.
//
// The browser supplies cookies and base information through the browser-Cdp
// extension. Everything else happens here: access tokens are minted in Go from
// those cookies, generations are submitted and polled directly against the Flow
// API, media is streamed to disk, and every outcome is recorded in SQLite.
package main

import (
	"os"

	"github.com/kodelyx/flow-go/flow-go/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
