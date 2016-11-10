package sseclient

import (
	"context"
	"log"
	"time"
)

func errorHandler(err error) bool {
	log.Printf("error : %s", err)
	return false
}

func eventHandler(event *Event) {
	log.Printf("event : %s : %s : %d bytes of data", event.ID, event.Event, len(event.Data))
}

func ExampleFull() {
	c := New("https://example.net/stream", "")
	ctx, _ := context.WithTimeout(context.Background(), time.Minute)
	c.Start(ctx, eventHandler, errorHandler)
}
