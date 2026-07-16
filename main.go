// cyclevpn — beat Russia's per-connection byte-quota throttle by spreading each
// TCP stream across many short-lived HTTPS requests (each under the quota).
//
//	cyclevpn relay   -listen 127.0.0.1:8791
//	cyclevpn client  -url https://EXIT_DOMAIN -listen 127.0.0.1:10900 -workers 24
//
// See README.md for the full two-hop deployment (phone -> RU entry -> exit).
package main

import (
	"fmt"
	"os"
)

const CHUNK = 14000 // bytes per request — safely under the ~16KB per-connection quota

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: cyclevpn <relay|client> [flags]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "relay":
		runRelay(os.Args[2:])
	case "client":
		runClient(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, "unknown mode:", os.Args[1])
		os.Exit(2)
	}
}
