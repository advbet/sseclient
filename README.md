sseclient
=========

[![GoDoc](https://godoc.org/bitbucket.org/advbet/sseclient?status.svg)](https://godoc.org/bitbucket.org/advbet/sseclient)

This is a go library for consuming streams [Server-Sent
Events](https://www.w3.org/TR/eventsource/ "SSE"). It handles automatic
reconnect and last seen event ID tracking.

Key differences:

- **Synchronous execution**. Reconnecting, event parsing and processing is
  executed in single go-routine that started the stream. This provides freedom
  to choose any concurrency and synchronization model.
- **Go context aware**. SSE streams can be optionally given a context on start.
  This gives flexibility to support different stream stopping mechanisms.

Usage example:

```go
package main

import (
        "context"
        "log"
        "time"

        "github.com/fln/sseclient"
)

func errorHandler(err error) bool {
        log.Printf("error : %s", err)
        return false
}

func eventHandler(event *sseclient.Event) {
        log.Printf("event : %s : %s : %d bytes", event.ID, event.Event, len(event.Data))
}

func main() {
        c := sseclient.New("https://example.net/stream", "")
        ctx, _ := context.WithTimeout(context.Background(), time.Minute)
        c.Start(ctx, eventHandler, errorHandler)
}
```
