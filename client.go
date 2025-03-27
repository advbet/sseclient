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
	"crypto/tls"
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
// an error including errors returned by EventHandler function. Network
// connection errors and response codes 500, 502, 503, 504 are not treated as
// errors.
//
// If error handler returns nil, error will be treated as handled and stream
// will continue to be processed (with automatic reconnect).
//
// If error handler returns error it is treated as fatal and stream processing
// loop exits returning received error up the stack.
//
// This handler can be used to implement complex error handling scenarios. For
// simple cases ReconnectOnError or StopOnError are provided by this library.
//
// Users of this package have to provide this function implementation.
type ErrorHandler func(error) error

// EventHandler is a callback that gets called every time event on the SSE
// stream is received. Error returned from handler function will be passed to
// the error handler.
//
// Users of this package have to provide this function implementation.
type EventHandler func(e *Event) error

// Client is used to connect to SSE stream and receive events. It handles HTTP
// request creation and reconnects automatically.
//
// Client struct should be created with New method or manually.
type Client struct {
	URL         string
	LastEventID string
	Retry       time.Duration
	HTTPClient  *http.Client
	Headers     http.Header

	// VerboseStatusCodes specifies whether connect should return all
	// status codes as errors if they're not StatusOK (200).
	VerboseStatusCodes bool
}

// List of commonly used error handler function implementations.
var (
	ReconnectOnError ErrorHandler = func(error) error { return nil }
	StopOnError      ErrorHandler = func(err error) error { return err }
)

var (
	// ErrMalformedEvent error is returned if stream ended with incomplete event.
	ErrMalformedEvent = errors.New("incomplete event at the end of the stream")

	// errStreamConn error is returned when client is unable to
	// connect to the stream. This error is only used to reconnect to
	// the stream without outputting connection errors to the client.
	errStreamConn = errors.New("cannot connect to the stream")
)

// New creates SSE stream client object. It will use given url and
// last event ID values and a 2-second retry timeout.
// It will use custom http client that skips verification for tls process.
// This method only creates Client struct and does not start connecting to the
// SSE endpoint.
func New(url, lastEventID string) *Client {
	return &Client{
		URL:         url,
		LastEventID: lastEventID,
		Retry:       2 * time.Second,
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true, //nolint:gosec
				},
			},
		},
		Headers: make(http.Header),
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
	errorFn := func(err error) error {
		select {
		case ch <- StreamMessage{Err: err}:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	eventFn := func(e *Event) error {
		select {
		case ch <- StreamMessage{Event: e}:
		case <-ctx.Done():
		}

		return nil
	}

	go func() {
		defer close(ch)
		c.Start(ctx, eventFn, errorFn)
	}()

	return ch
}

// Start connects to the SSE stream. This function will block until SSE stream
// is stopped. Stopping SSE stream is possible by cancelling given stream
// context or by returning some error from the error handler callback. Error
// returned by the error handler is passed back to the caller of this function.
func (c *Client) Start(ctx context.Context, eventFn EventHandler, errorFn ErrorHandler) error {
	lastTimeout := c.Retry / 32
	tm := time.NewTimer(0)

	stop := func() {
		tm.Stop()

		select {
		case <-tm.C:
		default:
		}
	}
	defer stop()

	for {
		err := c.connect(ctx, eventFn)
		switch {
		case err == nil, err == io.EOF:
			// ok, we can reconnect right away
			lastTimeout = c.Retry / 32
		case errors.Is(err, ctx.Err()):
			// context cancellation exits silently
			return nil
		default:
			if !errors.Is(err, errStreamConn) {
				if cerr := errorFn(err); cerr != nil {
					// error handler instructs to stop
					// the sse stream
					return cerr
				}
			}

			stop()
			tm.Reset(lastTimeout)

			select {
			case <-tm.C:
			case <-ctx.Done():
				// context cancellation exits silently
				return nil
			}

			if lastTimeout < c.Retry {
				lastTimeout = lastTimeout * 2
			}
		}
	}
}

// connect performs single connection to SSE endpoint.
func (c *Client) connect(ctx context.Context, eventFn EventHandler) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Accept", "text/event-stream")
	if c.LastEventID != "" {
		req.Header.Set("Last-Event-ID", c.LastEventID)
	}

	for h, vs := range c.Headers {
		for _, v := range vs {
			req.Header.Add(h, v)
		}
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		// silently ignore connection errors and reconnect.
		return errStreamConn
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// we do not support BOM in sse streams, or \r line separators.
		r := bufio.NewReader(resp.Body)
		for {
			event, err := c.parseEvent(r)
			if err != nil {
				return err
			}

			// ignore empty events
			if len(event.Data) == 0 {
				continue
			}

			if err := eventFn(event); err != nil {
				return err
			}
		}
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		if c.VerboseStatusCodes {
			return fmt.Errorf("bad response status code %d", resp.StatusCode)
		}

		// reconnect without logging an error.
		return errStreamConn
	default:
		// trigger a reconnect and output an error.
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
				err = ErrMalformedEvent
			}
			return nil, err
		}

		if len(line) == 0 {
			c.LastEventID = event.ID
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
