// Package universal provides a way to express an atomic unit of work that can be used
// as a message handler and an HTTP handler.
// TODO: Also rpc!
// TODO: All in separate goroutines!
package universal

import (
	"bytes"
	"context"
	"encoding"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/pkg/errors"
)

// Assert that *httpMessage satisfies the http.ResponseWriter interface.
var _ http.ResponseWriter = &httpMessage{}

var errTimeout = errors.New("stop timeout reached")

// Message is a task to be completed by a Processor.
type Message interface {
	Context() context.Context
	// Reader provides the Message input as a readable byte stream.
	io.Reader
	// Writer provides the Message output as a writable byte stream.
	io.Writer
}

// httpMessage is a Message representation of an HTTP request. It satisfies both
// the Message interface and the http.ResponseWriter interface, allowing it to
// be treated as a generic message processor response or as an HTTP-specific response.
type httpMessage struct {
	request    *http.Request
	output     *bytes.Buffer
	header     http.Header
	statusCode int
}

func newHTTPMessage(request *http.Request) *httpMessage {
	return &httpMessage{
		request: request,
		// TODO: Consider using a sync.Pool of buffers instead of creating a new
		// one for every request.
		output: new(bytes.Buffer),
		header: make(http.Header),
	}
}

func (msg *httpMessage) Context() context.Context {
	return msg.request.Context()
}

func (msg *httpMessage) Read(p []byte) (int, error) {
	return msg.request.Body.Read(p)
}

func (msg *httpMessage) Write(p []byte) (int, error) {
	return msg.output.Write(p)
}

func (msg *httpMessage) Header() http.Header {
	return msg.header
}

func (msg *httpMessage) WriteHeader(statusCode int) {
	msg.statusCode = statusCode
}

// An HTTPError provides HTTP-specific response information for an error. It allows
// setting a specific HTTP status code, header values, and the response body.
type HTTPError interface {
	StatusCode() int
	Header() http.Header
	encoding.BinaryMarshaler
	error
}

// A Processor implements all logic for handling incoming Messages, either from
// a Service.Run loop or from a Service's HTTP handler.
type Processor interface {
	// Process accepts a Message, performing all actions necessary to complete
	// the processing of the Message except for persistence of the result to the
	// output data system (e.g. a message queue or a database). The Messages come
	// from either a Service.Run loop or a Service's HTTP handler. All Process calls
	// run in separate goroutines.
	//
	// Any error returned is be passed to the Finish function. If the Message
	// came from an HTTP request, the error is returned to the caller in the
	// HTTP body.
	Process(Message) error

	// Finish handles persistence of the results of processing a Message to the
	// output data system (e.g. a message queue or a database). The processed
	// Message and any error returned while processing the Message are passed
	// to Finish.
	//
	// Finish is only called from a Service.Run loop and is not called for Messages
	// from a Service's HTTP handler. All Finish calls run in separate goroutines
	// (in the same goroutine as the Process call).
	Finish(Message, error)
}

type Service struct {
	processor Processor
	wg        sync.WaitGroup
}

func NewService(processor Processor) *Service {
	return &Service{processor: processor}
}

// Run calls Process then Finish on all messages returned by the provided Message
// source function, blocking until the provided Message source function returns
// nil.
//
// When the provided Message source returns a nil Message, Run starts to shut
// down, blocking until all running goroutines started by Run have returned or
// until the shutdownTimeout is reached, whichever comes first. If the
// shutdownTimeout is reached, Run returns an error.
func (svc *Service) Run(next func() Message, shutdownTimeout time.Duration) error {
	for {
		msg := next()
		if msg == nil {
			break
		}
		svc.wg.Add(1)
		go svc.processAndFinish(msg)
	}
	// Wait until the WaitGroup is done, indicating that all started goroutines
	// have returned, or until the provided Context is cancelled. Return any
	// error from the Context, allowing the caller to detect an unclean shutdown.
	done := make(chan struct{})
	go func() {
		svc.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shutdownTimeout):
		return errTimeout
	}

	return nil
}

func (svc *Service) processAndFinish(msg Message) {
	err := svc.processor.Process(msg)
	svc.processor.Finish(msg, err)
	svc.wg.Done()
}

// ServeHTTP is an HTTP handler that calls the Service's Process function with
// the HTTP request as a Message. All body unmarshalling and response marshalling
// is performed by the Process function, just like in the Service.Run loop.
func (svc *Service) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	defer request.Body.Close()
	// Wrap the HTTP request in a type that satisfies the Message interface and
	// call Process with that wrapped HTTP response.
	message := newHTTPMessage(request)
	err := svc.processor.Process(message)
	// Check if the returned error satisfies the HTTPError interface. If so,
	// type-assert to HTTPError and use the additional information to write an
	// HTTP-specific response instead of a generic HTTP response.
	if httpErr, ok := err.(HTTPError); ok {
		b, err := httpErr.MarshalBinary()
		if err != nil {
			http.Error(
				writer,
				fmt.Sprintf("error marshalling HTTPError as binary: %s", err),
				http.StatusInternalServerError)
			return
		}
		for key, values := range httpErr.Header() {
			for _, value := range values {
				writer.Header().Add(key, value)
			}
		}
		writer.WriteHeader(httpErr.StatusCode())
		_, err = writer.Write(b)
		if err != nil {
			http.Error(
				writer,
				fmt.Sprintf("error writing HTTP response from HTTPError: %s", err),
				http.StatusInternalServerError)
			return
		}
		return
	}
	// If there was any other type of error, return an HTTP 500 with the error
	// message as the body.
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	// If Process set any HTTP header values to the httpMessage, copy them to the
	// ResultWriter.
	for key, values := range message.header {
		for _, value := range values {
			writer.Header().Add(key, value)
		}
	}
	// Don't call WriteHeader until the header values are set on the writer or
	// the header values won't be written!
	if message.statusCode != 0 {
		writer.WriteHeader(message.statusCode)
	}
	_, err = writer.Write(message.output.Bytes())
	if err != nil {
		http.Error(
			writer,
			fmt.Sprintf("error writing HTTP response: %s", err),
			http.StatusInternalServerError)
		return
	}
}
