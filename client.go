// Package sseclient is library for consuming SSE streams.
//
// Key features:
//
// Synchronous execution. Reconnecting, event parsing and processing is executed
// in single go-routine that started the stream. This gives freedom to use any
// concurrency and synchronization model.
//
// Go context aware. SSE streams can be optionally given a context on start.
// This gives flexibility to support different stream stopping mechanisms.
package sseclient

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Event object is a representation of single chunk of data in event stream.
type Event struct {
	ID    string
	Event string
	Data  []byte
}

// ErrorHandler is a callback that gets called every time SSE stream encounters
// an error. Network connection errors and response codes 500, 502, 503,
// 504 are not treated as errors.
//
// Error handler function should return true if SSE stream should be stopped or
// false if SSE stream should reconnect.
//
// Users of this package have to provide this function implementation.
type ErrorHandler func(error) (stop bool)

// EventHandler is a callback that gets called every time event on the SSE
// stream is received.
//
// Users of this package have to provide this function implementation.
type EventHandler func(e *Event)

// Client is used to connect to SSE stream and receive events. It handles HTTP
// request creation and reconnects automatically.
//
// Client struct should be created with New method or manually.
type Client struct {
	URL         string
	LastEventID string
	Retry       time.Duration
	HTTPClient  *http.Client
}

// List of commonly used error handler function implementations.
var (
	ReconnectOnError ErrorHandler = func(error) bool { return false }
	StopOnError      ErrorHandler = func(error) bool { return true }
)

// MalformedEvent error is returned if stream ended with incomplete event.
var MalformedEvent = errors.New("incomplete event at the end of the stream")

// New creates SSE stream client object. It will use given URL and last event ID
// values, default HTTP client from http package and 2 second retry timeout.
// This method only creates Client struct and does not start connecting to the
// SSE endpoint.
func New(url, lastEventID string) *Client {
	return &Client{
		URL:         url,
		LastEventID: lastEventID,
		Retry:       2 * time.Second,
		HTTPClient:  http.DefaultClient,
	}
}

// StreamMessage stores single SSE event or error.
type StreamMessage struct {
	Event *Event
	Err   error
}

// Stream is non-blocking SSE stream consumption mode where events are passed
// through a channel. Stream can be stopped by cancelling context.
//
// Parameter buf controls returned stream channel buffer size. Buffer size of 0
// is a good default.
func (c *Client) Stream(ctx context.Context, buf int) <-chan StreamMessage {
	ch := make(chan StreamMessage, buf)
	errorFn := func(err error) bool {
		ch <- StreamMessage{Err: err}
		return false
	}
	eventFn := func(e *Event) {
		ch <- StreamMessage{Event: e}
	}
	go func() {
		defer close(ch)
		c.Start(ctx, eventFn, errorFn)
	}()
	return ch
}

// Start connects to the SSE stream. This function will block until SSE stream
// is stopped. Stopping SSE stream is possible by cancelling given stream
// context or by returning true from the error handler callback.
func (c *Client) Start(ctx context.Context, eventFn EventHandler, errorFn ErrorHandler) {
	for {
		if err := c.connect(ctx, eventFn); err != nil && err != io.EOF {
			stop := errorFn(err)
			if stop {
				// Error handler instructs to stop SSE stream
				break
			}
		}
		if ctx != nil && ctx.Err() != nil {
			// Someone cancelled the context, exit silently
			break
		}
		time.Sleep(c.Retry)
	}
}

// connect performs single connection to SSE endpoint.
func (c *Client) connect(ctx context.Context, eventFn EventHandler) error {
	req, err := http.NewRequest(http.MethodGet, c.URL, nil)
	if err != nil {
		return err
	}
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Accept", "text/event-stream")
	if c.LastEventID != "" {
		req.Header.Set("Last-Event-ID", c.LastEventID)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		// silently ignore connection errors and reconnect
		return nil
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// we do not support BOM in sse streams, or \r line separators
		r := bufio.NewReader(resp.Body)
		for {
			event, err := c.parseEvent(r)
			if err != nil {
				// io.EOF error will reconnect sliently, others
				// will be passed to error handler
				return err
			}
			// ignore empty events
			if len(event.Data) == 0 {
				continue
			}
			eventFn(event)
		}
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		// reconnect without logginng an error
		return nil
	default:
		// trigger error + reconnect
		return fmt.Errorf("bad response status code %d", resp.StatusCode)
	}
}

// chomp removes \r or \n or \r\n suffix from the given byte slice.
func chomp(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	if len(b) > 0 && b[len(b)-1] == '\r' {
		b = b[:len(b)-1]
	}
	return b
}

// parseEvent reads a single Event fromthe event stream.
func (c *Client) parseEvent(r *bufio.Reader) (*Event, error) {
	event := &Event{
		ID:    c.LastEventID,
		Event: "message",
	}
	for {
		line, err := r.ReadBytes('\n')
		line = chomp(line) // its ok to chop nil slice
		if err != nil {
			// EOF is treated as silent reconnect. If this is
			// malformed event report an error.
			if err == io.EOF && len(line) != 0 {
				err = MalformedEvent
			}
			return nil, err
		}

		if len(line) == 0 {
			return event, nil
		}
		parts := bytes.SplitN(line, []byte(":"), 2)

		// Make sure parts[1] always exist
		if len(parts) == 1 {
			parts = append(parts, nil)
		}

		// Chomp space after ":"
		if len(parts[1]) > 0 && parts[1][0] == ' ' {
			parts[1] = parts[1][1:]
		}
		switch string(parts[0]) {
		case "retry":
			ms, err := strconv.Atoi(string(parts[1]))
			if err != nil {
				continue
			}
			c.Retry = time.Duration(ms) * time.Millisecond
		case "id":
			event.ID = string(parts[1])
			c.LastEventID = string(parts[1])
		case "event":
			event.Event = string(parts[1])
		case "data":
			if event.Data != nil {
				event.Data = append(event.Data, '\n')
			}
			event.Data = append(event.Data, parts[1]...)
		default:
			// Ignore unknown fields and comments
			continue
		}
	}
}
