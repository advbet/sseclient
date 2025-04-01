package main

import (
	"context"
	"log"
	"time"

	"github.com/advbet/sseclient"
)

func errorHandler(err error) error {
	log.Printf("error : %s", err)
	return nil
}

func eventHandler(event *sseclient.Event) error {
	log.Printf("event : %s : %s : %d bytes of data", event.ID, event.Event, len(event.Data))
	return nil
}

func main() {
	c := sseclient.New("https://example.net/stream", "")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c.Start(ctx, eventHandler, errorHandler)
}
