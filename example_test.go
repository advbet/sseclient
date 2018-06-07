package sseclient

import (
	"context"
	"log"
	"time"
)

func errorHandler(err error) error {
	log.Printf("error : %s", err)
	return nil
}

func eventHandler(event *Event) error {
	log.Printf("event : %s : %s : %d bytes of data", event.ID, event.Event, len(event.Data))
	return nil
}

func Example() {
	c := New("https://example.net/stream", "")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c.Start(ctx, eventHandler, errorHandler)
}
