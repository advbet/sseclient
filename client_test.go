package sseclient

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestParseEventRetry(t *testing.T) {
	t.Parallel()

	r := bufio.NewReader(bytes.NewBufferString("retry: 100\n\n"))
	client := &Client{}

	_, err := client.parseEvent(r)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if client.Retry != 100*time.Millisecond {
		t.Fatalf("expected retry to be 100ms, got %v", client.Retry)
	}
}

func TestParseEventInvalidRetry(t *testing.T) {
	t.Parallel()

	r := bufio.NewReader(bytes.NewBufferString("retry: ???\n\n"))
	client := &Client{}

	_, err := client.parseEvent(r)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if client.Retry != 0 {
		t.Fatalf("expected retry to be 0, got %v", client.Retry)
	}
}

func TestParseEvent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		data  string
		event *Event
		err   error
	}{
		{
			data: "\n\n",
			event: &Event{
				ID:    "",
				Event: "message",
				Data:  nil,
			},
			err: nil,
		},
		{
			data: "id: 123\n\n",
			event: &Event{
				ID:    "123",
				Event: "message",
				Data:  nil,
			},
			err: nil,
		},
		{
			data: "event: create\n\n",
			event: &Event{
				ID:    "",
				Event: "create",
				Data:  nil,
			},
			err: nil,
		},
		{
			data: "data: some data\n\n",
			event: &Event{
				ID:    "",
				Event: "message",
				Data:  []byte("some data"),
			},
			err: nil,
		},
		{
			data: "data: some data\ndata: multiline data\n\n",
			event: &Event{
				ID:    "",
				Event: "message",
				Data:  []byte("some data\nmultiline data"),
			},
			err: nil,
		},
		{
			data: "data: some data\r\ndata: multiline data\r\n\r\n",
			event: &Event{
				ID:    "",
				Event: "message",
				Data:  []byte("some data\nmultiline data"),
			},
			err: nil,
		},
		{
			data: ": some comment\n\n",
			event: &Event{
				ID:    "",
				Event: "message",
				Data:  nil,
			},
			err: nil,
		},
		{
			data: "unsupported field\n\n",
			event: &Event{
				ID:    "",
				Event: "message",
				Data:  nil,
			},
			err: nil,
		},
		{
			data: "id:123\nevent:create\ndata:this is some data\n\n",
			event: &Event{
				ID:    "123",
				Event: "create",
				Data:  []byte("this is some data"),
			},
			err: nil,
		},
		{
			data: "id: 123\nevent: create\ndata: this is some data\n\n",
			event: &Event{
				ID:    "123",
				Event: "create",
				Data:  []byte("this is some data"),
			},
			err: nil,
		},
		{
			data: `id: 123
event: create
data: this is some data
unsupported field
: some comment
data: multiline data

`,
			event: &Event{
				ID:    "123",
				Event: "create",
				Data:  []byte("this is some data\nmultiline data"),
			},
			err: nil,
		},
		{
			data:  "data: test", // missing \n to be complete event
			event: nil,
			err:   ErrMalformedEvent,
		},
		{
			data:  "",
			event: nil,
			err:   io.EOF,
		},
		{
			data:  "data: test\n",
			event: nil,
			err:   io.EOF,
		},
	}

	for _, test := range tests {
		r := bufio.NewReader(bytes.NewBufferString(test.data))
		client := &Client{}

		event, err := client.parseEvent(r)
		if !reflect.DeepEqual(event, test.event) {
			t.Fatalf("expected event %v, got %v", test.event, event)
		}

		if !errors.Is(err, test.err) {
			t.Fatalf("expected error %v, got %v", test.err, err)
		}
	}
}

func sseHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/single-event", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = fmt.Fprint(w, "data: singe event stream\n\n")
	})

	mux.HandleFunc("/500", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "oops 500", http.StatusInternalServerError)
	})

	mux.HandleFunc("/409", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "oops 409", http.StatusConflict)
	})

	return mux
}

func TestClientReconnect(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(sseHandler())
	defer server.Close()

	// single event stream will disconnect after emitting single event, sse
	// client should automatically reconnect until context deadline stops it
	client := New(server.URL+"/single-event", "xxx")
	client.Retry = 0

	counter := 0
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	handler := func(*Event) error {
		counter++
		if counter == 5 {
			cancel()
		}

		return nil
	}

	_ = client.Start(ctx, handler, ReconnectOnError)

	// We must have at least 2 reconnect attempts to confirm that client
	// reconnected automatically
	if counter != 5 {
		t.Fatalf("expected to receive 5 events, received %d", counter)
	}
}

func TestClientError409(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(sseHandler())
	defer server.Close()

	ok := false
	eventHandler := func(*Event) error { return nil }
	errorHandler := func(error) error {
		ok = true

		return errors.New("stop")
	}

	// /409 endpoint will return 409 status code which should trigger an
	// error. If out error handler catches the error it will mark test as
	// successful and stop sse client
	client := New(server.URL+"/409", "")

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()

	_ = client.Start(ctx, eventHandler, errorHandler)

	// We must have at least 2 reconnect attempts to confirm that client
	// reconnected automatically
	if !ok {
		t.Fatalf("response code 409 should trigger a call to error handler")
	}
}

func TestClientEventHandlerErrorPropagation(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(sseHandler())
	defer server.Close()

	parserErr := errors.New("fail always")
	streamErr := errors.New("stop the stream")

	var receivedByHandler error

	eventHandler := func(*Event) error { return parserErr }
	errorHandler := func(err error) error {
		receivedByHandler = err

		return streamErr
	}

	// /single-event endpoint will emit single event but our handler will
	// fail to parse it. We check if error returned by parser is passed back
	// to the error handler and if error returned by error handler is passed
	// back on stream end.
	client := New(server.URL+"/single-event", "")

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()

	err := client.Start(ctx, eventHandler, errorHandler)
	if !errors.Is(err, streamErr) {
		t.Fatalf("stream client dropped error handler error")
	}

	if !errors.Is(receivedByHandler, parserErr) {
		t.Fatalf("stream client did not pass parser error to error handler")
	}
}

func TestClientStream(t *testing.T) {
	t.Parallel()

	expected := []StreamMessage{{
		Event: &Event{
			Event: "message",
			Data:  []byte("singe event stream"),
		},
	}}

	server := httptest.NewServer(sseHandler())
	defer server.Close()

	client := New(server.URL+"/single-event", "")
	client.Retry = 0

	ctx, stop := context.WithCancel(t.Context())
	defer stop()

	actual := make([]StreamMessage, 0, len(expected))

	for msg := range client.Stream(ctx, 0) {
		actual = append(actual, msg)

		stop()
	}

	if !reflect.DeepEqual(expected, actual) {
		t.Errorf("expected %v, got %v", expected, actual)
	}
}

func TestClientStreamError(t *testing.T) {
	t.Parallel()

	expected := []StreamMessage{{
		Err: errors.New("bad response status code: 409"),
	}}

	server := httptest.NewServer(sseHandler())
	defer server.Close()

	client := New(server.URL+"/409", "")
	client.Retry = 0

	ctx, stop := context.WithCancel(t.Context())
	defer stop()

	actual := make([]StreamMessage, 0, len(expected))

	for msg := range client.Stream(ctx, 0) {
		actual = append(actual, msg)

		stop()
	}

	if !reflect.DeepEqual(expected, actual) {
		t.Errorf("expected %v, got %v", expected, actual)
	}
}

func TestReconnectAfterPartialEvent(t *testing.T) {
	t.Parallel()

	expected := []StreamMessage{
		{
			Event: &Event{
				ID:    "1",
				Event: "message",
				Data:  []byte("message1"),
			},
		},
		{
			Err: ErrMalformedEvent,
		},
		{
			Event: &Event{
				ID:    "2",
				Event: "message",
				Data:  []byte("message2"),
			},
		},
		{
			Event: &Event{
				ID:    "3",
				Event: "message",
				Data:  []byte("message3"),
			},
		},
	}

	ctx, stop := context.WithCancel(t.Context())
	defer stop()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")

		var response string

		id := r.Header.Get("Last-Event-ID")
		switch id {
		case "0": // first request
			response = "id: 1\ndata: message1\n\nid: 2\ndata: partial second message"
		case "1": // second request
			response = "id: 2\ndata: message2\n\n"
		case "2": // third request
			response = "id: 3\ndata: message3\n\n"
		default:
			stop()
		}

		_, _ = fmt.Fprint(w, response)
	}))
	defer server.Close()

	client := New(server.URL, "0")
	client.Retry = 0

	actual := make([]StreamMessage, 0, len(expected))
	for msg := range client.Stream(ctx, 0) {
		actual = append(actual, msg)
	}

	if !reflect.DeepEqual(expected, actual) {
		t.Errorf("expected %v, got %v", expected, actual)
	}
}
