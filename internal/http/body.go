package http

import "net/http"

// MaxBodyBytes bounds every request body. The largest legitimate one is a
// purchase, well under 1 KiB.
const MaxBodyBytes = 64 << 10

// LimitBody makes reads past MaxBodyBytes fail, so no handler can be made to
// buffer an arbitrarily large request.
func LimitBody(h http.Handler) http.Handler { return http.MaxBytesHandler(h, MaxBodyBytes) }
