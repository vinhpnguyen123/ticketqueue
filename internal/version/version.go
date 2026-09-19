// Package version carries the build identity of the running binary.
//
// SHA is injected at link time. Nothing sets it in a `go run` or `go test`
// build, which is why the default is "dev" rather than an empty string: a
// /health response saying "dev" is unambiguous, one saying "" looks like a bug
// in the handler.
//
//	go build -ldflags="-X github.com/vinhpnguyen123/ticketqueue/internal/version.SHA=$(git rev-parse --short HEAD)"
//
// Why this exists at all: once three images are running in ECS, "which commit
// is this?" is the first question when something behaves oddly, and the
// container tag does not answer it — a tag can be re-pushed, and `latest`
// always lies.
package version

import (
	"net"
	"net/http"
	"time"
)

// SHA is the git commit this binary was built from. Overridden via -ldflags.
var SHA = "dev"

// Healthcheck probes a URL and reports whether it answered 2xx. It backs the
// `-healthcheck` flag every binary carries.
//
// Why a flag on the binary instead of curl in the image: the runtime image is
// distroless, which has no shell and no HTTP client, so a Docker HEALTHCHECK
// has nothing to run. Shipping the probe inside the binary keeps the image at
// two files (the binary and CA certs) and gives ECS the same check the local
// compose stack uses. It exits 0 or 1 and prints nothing on success.
func Healthcheck(url string) int {
	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			DialContext:       (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
			DisableKeepAlives: true,
		},
	}
	resp, err := client.Get(url)
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return 1
	}
	return 0
}
