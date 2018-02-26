package universal

import (
	"encoding/json"
	"errors"
	"io/ioutil"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testProcessor struct {
	process func(Message) error
	finish  func(Message, error)
}

func (tp testProcessor) Process(m Message) error {
	return tp.process(m)
}

func (tp testProcessor) Finish(m Message, err error) {
	tp.finish(m, err)
}

func TestService_Run(t *testing.T) {
	// TODO: Run tests.
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
