package universal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testMessage struct {
	input  io.Reader
	result *bytes.Buffer
}

func newTestMessage(msg string) *testMessage {
	return &testMessage{
		input:  strings.NewReader(msg),
		result: new(bytes.Buffer),
	}
}

func (msg *testMessage) Context() context.Context    { return context.TODO() }
func (msg *testMessage) Read(p []byte) (int, error)  { return msg.input.Read(p) }
func (msg *testMessage) Write(p []byte) (int, error) { return msg.result.Write(p) }

type testProcessor struct {
	process func(Message) error
	finish  func(Message, error)
}

func (proc testProcessor) Process(m Message) error     { return proc.process(m) }
func (proc testProcessor) Finish(m Message, err error) { proc.finish(m, err) }

func makeMessages(msg string, n int) []Message {
	messages := make([]Message, n)
	for i := 0; i < len(messages)-1; i++ {
		messages[i] = newTestMessage(msg)
	}
	messages[len(messages)-1] = nil
	return messages
}

func TestService_Run(t *testing.T) {
	tests := []struct {
		description string
		messages    []Message
		timeout     time.Duration
		process     func(*testing.T, *uint64) func(Message) error
		finish      func(*testing.T, *uint64) func(Message, error)
		wantErr     error
	}{
		{
			description: "All available messages should be passed to Process then Finish",
			messages:    makeMessages("test message", 100),
			timeout:     1 * time.Second,
			process: func(t *testing.T, i *uint64) func(Message) error {
				return func(m Message) error {
					b, err := ioutil.ReadAll(m)
					require.NoError(t, err, "Error reading message")
					assert.Equal(
						t,
						"test message",
						string(b),
						"Expected and actual messages are different")
					_, err = m.Write([]byte("test result"))
					require.NoError(t, err, "Error writing message result")
					atomic.AddUint64(i, 1)
					return nil
				}
			},
			finish: func(t *testing.T, i *uint64) func(Message, error) {
				return func(m Message, procErr error) {
					require.NoError(t, procErr, "Error from Process")
					message := m.(*testMessage)
					assert.Equal(
						t,
						"test result",
						string(message.result.Bytes()),
						"Expected and actual Process results are different")
					atomic.AddUint64(i, 1)
				}
			},
		},
		{
			description: "Any error returned by Process should be passed to Finish",
			messages:    makeMessages("", 100),
			timeout:     1 * time.Second,
			process: func(t *testing.T, i *uint64) func(Message) error {
				return func(m Message) error {
					atomic.AddUint64(i, 1)
					return errors.New("test error")
				}
			},
			finish: func(t *testing.T, i *uint64) func(Message, error) {
				return func(m Message, procErr error) {
					assert.EqualError(
						t,
						procErr,
						"test error",
						"Expected and actual Process errors are different")
					atomic.AddUint64(i, 1)
				}
			},
		},
		{
			description: "Run should wait for all started goroutines to complete before returning",
			messages:    makeMessages("", 10000),
			timeout:     1 * time.Second,
			process: func(t *testing.T, i *uint64) func(Message) error {
				return func(m Message) error {
					time.Sleep(10 * time.Millisecond)
					atomic.AddUint64(i, 1)
					return nil
				}
			},
			finish: func(t *testing.T, i *uint64) func(Message, error) {
				return func(m Message, procErr error) {
					time.Sleep(10 * time.Millisecond)
					atomic.AddUint64(i, 1)
				}
			},
		},
		{
			description: "If the first message is nil, Run should exit immediately without calling Process or Finish",
			messages:    []Message{nil},
			timeout:     1 * time.Second,
			process:     func(*testing.T, *uint64) func(Message) error { return nil },
			finish:      func(*testing.T, *uint64) func(Message, error) { return nil },
		},
		{
			description: "If the shutdown timeout is reached, Run should return an error",
			messages:    []Message{newTestMessage(""), nil},
			timeout:     0,
			process: func(*testing.T, *uint64) func(Message) error {
				return func(Message) error {
					time.Sleep(1 * time.Hour)
					return nil
				}
			},
			finish: func(*testing.T, *uint64) func(Message, error) {
				return func(Message, error) {
					time.Sleep(1 * time.Hour)
				}
			},
			wantErr: errTimeout,
		},
	}
	for _, test := range tests {
		test := test // Capture range variable.
		t.Run(test.description, func(t *testing.T) {
			t.Parallel()

			var processCount uint64
			var finishCount uint64
			svc := NewService(testProcessor{
				process: test.process(t, &processCount),
				finish:  test.finish(t, &finishCount),
			})
			// Make a copy of the test.messages slice header so we don't modify the
			// test inputs. We're just re-slicing the messages slice to "pop" messages
			// off the top, so it won't modify the referenced Messages.
			messages := test.messages
			err := svc.Run(func() Message {
				msg := messages[0]
				messages = messages[1:len(messages)]
				return msg
			}, test.timeout)
			require.Equal(t, test.wantErr, err, "Expected and actual errors are different")
			// The number of Process and Finish calls is undefined when Run reaches
			// the shutdown timeout, so return if Run returns the timeout error.
			if err == errTimeout {
				return
			}
			assert.Equal(
				t,
				uint64(len(test.messages)-1),
				processCount,
				"Expected and actual number of Process calls are different")
			assert.Equal(
				t,
				uint64(len(test.messages)-1),
				finishCount,
				"Expected and actual number of Finish calls are different")
		})
	}
}

type testHTTPError struct {
	error
}

func (err testHTTPError) StatusCode() int {
	return http.StatusNotImplemented
}

func (err testHTTPError) Header() http.Header {
	return http.Header{"Content-Type": []string{"application/json"}}
}

func (err testHTTPError) MarshalBinary() ([]byte, error) {
	return json.Marshal(map[string]string{"error": err.Error()})
}

func TestService_ServeHTTP(t *testing.T) {
	tests := []struct {
		description string
		request     func(net.Addr) (*http.Request, error)
		process     func(*testing.T) func(Message) error
		wantStatus  int
		wantHeader  http.Header
		wantBody    []byte
	}{
		{
			description: "An HTTP POST request should call the processor with the HTTP request body",
			request: func(addr net.Addr) (*http.Request, error) {
				return http.NewRequest(
					http.MethodPost,
					"http://"+addr.String(),
					strings.NewReader("test message"))
			},
			process: func(t *testing.T) func(Message) error {
				return func(m Message) error {
					message, err := ioutil.ReadAll(m)
					require.NoError(t, err, "Error reading message")
					assert.Equal(
						t,
						[]byte("test message"),
						message,
						"Expected and actual messages are different")
					_, err = m.Write([]byte("test result"))
					require.NoError(t, err, "Error writing message result")
					return nil
				}
			},
			wantStatus: http.StatusOK,
			wantBody:   []byte("test result"),
		},
		{
			description: "An HTTP GET request should call the processor with no HTTP request body",
			request: func(addr net.Addr) (*http.Request, error) {
				return http.NewRequest(
					http.MethodGet,
					"http://"+addr.String(),
					nil)
			},
			process: func(t *testing.T) func(Message) error {
				return func(m Message) error {
					message, err := ioutil.ReadAll(m)
					require.NoError(t, err, "Error reading message")
					assert.Equal(
						t,
						[]byte{},
						message,
						"Expected and actual messages are different")
					_, err = m.Write([]byte("test result"))
					require.NoError(t, err, "Error writing message result")
					return nil
				}
			},
			wantStatus: http.StatusOK,
			wantBody:   []byte("test result"),
		},
		{
			description: "If the message is used as a ResponseWriter, the order calls to the ResponseWriter should not affect the HTTP response",
			request: func(addr net.Addr) (*http.Request, error) {
				return http.NewRequest(
					http.MethodPost,
					"http://"+addr.String(),
					nil)
			},
			process: func(t *testing.T) func(Message) error {
				return func(m Message) error {
					writer := m.(http.ResponseWriter)
					// Call Write, then WriteHeader, then add HTTP header values,
					// which would cause the status code and HTTP header values
					// to be dropped if we were writing directly to the ResponseWriter
					// provided by the ServeHTTP function.
					writer.Write([]byte("http result"))
					writer.WriteHeader(http.StatusTeapot)
					writer.Header().Set("Whats-For-Dinner", "peas")
					writer.Header().Set("Content-Type", "tea/green")
					return nil
				}
			},
			wantStatus: http.StatusTeapot,
			wantHeader: http.Header{
				"Content-Type":     []string{"tea/green"},
				"Whats-For-Dinner": []string{"peas"},
			},
			wantBody: []byte("http result"),
		},
		{
			description: "An HTTP POST request with a JSON request should be decoded successfully",
			request: func(addr net.Addr) (*http.Request, error) {
				return http.NewRequest(
					http.MethodPost,
					"http://"+addr.String(),
					strings.NewReader(`{"a": 9, "b": 16}`))
			},
			process: func(t *testing.T) func(Message) error {
				return func(m Message) error {
					var message struct {
						A int `json:"a"`
						B int `json:"b"`
					}
					err := json.NewDecoder(m).Decode(&message)
					require.NoError(t, err, "Error decoding message as JSON")
					result := struct {
						Result int `json:"result"`
					}{
						Result: message.A + message.B,
					}
					err = json.NewEncoder(m).Encode(&result)
					require.NoError(t, err, "Error encoding result as JSON")
					m.(http.ResponseWriter).Header().Set("Content-Type", "application/json")
					return nil
				}
			},
			wantStatus: http.StatusOK,
			wantHeader: http.Header{
				"Content-Type": []string{"application/json"},
			},
			// The newline character at the end is written by json.NewEncoder.
			// json.Marshal does not write a newline character at the end, but
			// it makes the code cleaner to use json.NewEncoder.
			wantBody: []byte(`{"result":25}` + "\n"),
		},
		{
			description: "If an error is returned, any Message writes should be ignored and the HTTP response body should include the error",
			request: func(addr net.Addr) (*http.Request, error) {
				return http.NewRequest(
					http.MethodPost,
					"http://"+addr.String(),
					nil)
			},
			process: func(t *testing.T) func(Message) error {
				return func(m Message) error {
					_, err := m.Write([]byte("test result"))
					require.NoError(t, err, "Error writing message result")
					return errors.New("test error")
				}
			},
			wantStatus: http.StatusInternalServerError,
			// The newline character at the end is written by http.Error, which
			// uses fmt.Fprintln to write the error to the ResponseWriter.
			wantBody: []byte("test error\n"),
		},
		{
			description: "If an error is returned that satisfies the HTTPError interface, the HTTP-specific information in the error should be used to write the response",
			request: func(addr net.Addr) (*http.Request, error) {
				return http.NewRequest(
					http.MethodPost,
					"http://"+addr.String(),
					nil)
			},
			process: func(t *testing.T) func(Message) error {
				return func(m Message) error {
					return testHTTPError{errors.New("not implemented")}
				}
			},
			wantStatus: http.StatusNotImplemented,
			wantHeader: http.Header{"Content-Type": []string{"application/json"}},
			wantBody:   []byte(`{"error":"not implemented"}`),
		},
	}
	for _, test := range tests {
		test := test // Capture range variable.
		t.Run(test.description, func(t *testing.T) {
			t.Parallel()

			svc := NewService(testProcessor{
				process: test.process(t),
				// Don't pass any finish function because we don't expect it to
				// be called. Any unexpected calls will cause a panic.
			})

			// Create a TCP listener on a random port on the loopback address
			// and start an HTTP server using that listener that serves the
			// Service's HTTP handler function.
			l, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err, "Error creating TCP listener")
			go http.Serve(l, svc)
			defer l.Close()

			req, err := test.request(l.Addr())
			require.NoError(t, err, "Error getting HTTP request")
			got, err := http.DefaultClient.Do(req)
			require.NoError(t, err, "Error calling HTTP server")
			defer got.Body.Close()

			assert.Equal(
				t,
				test.wantStatus,
				got.StatusCode,
				"Expected and actual HTTP status codes are different")

			// Test that the wanted HTTP header values are present. Ignore any
			// additional HTTP header values set by the HTTP server.
			for key := range test.wantHeader {
				assert.Equalf(
					t,
					test.wantHeader[key],
					got.Header[key],
					"Expected and actual HTTP header are different for key %q",
					key)
			}

			gotBody, err := ioutil.ReadAll(got.Body)
			require.NoError(t, err, "Error reading HTTP response body")
			assert.Equal(
				t,
				test.wantBody,
				gotBody,
				"Expected and actual HTTP body are different")
		})
	}
}
