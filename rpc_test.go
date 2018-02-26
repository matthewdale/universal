package universal

import (
	"context"
	"errors"
	"net/rpc"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type Args struct {
	A, B int
	Err  error
}

type Arith int

func (t *Arith) Multiply(args *Args, reply *int) error {
	*reply = args.A * args.B
	return args.Err
}

type testRPCMessage struct {
	request rpc.Request
	args    interface{}
}

func newTestRPCMessage(method string, args interface{}) *testRPCMessage {
	return &testRPCMessage{
		request: rpc.Request{ServiceMethod: method},
		args:    args,
	}
}

func (msg *testRPCMessage) Context() context.Context { return context.TODO() }
func (msg *testRPCMessage) Request() rpc.Request     { return msg.request }
func (msg *testRPCMessage) Args() interface{}        { return msg.args }

type testFinisher struct {
	finish func(RPCMessage, RPCResult, error)
}

func (proc testFinisher) Finish(m RPCMessage, r RPCResult, err error) {
	proc.finish(m, r, err)
}

func makeRPCMessages(method string, args interface{}, n int) []RPCMessage {
	messages := make([]RPCMessage, n)
	for i := 0; i < len(messages)-1; i++ {
		messages[i] = newTestRPCMessage(method, args)
	}
	messages[len(messages)-1] = nil
	return messages
}

func TestRPCService_Run(t *testing.T) {
	tests := []struct {
		description string
		messages    []RPCMessage
		timeout     time.Duration
		finish      func(*testing.T, *uint64) func(RPCMessage, RPCResult, error)
		wantErr     error
	}{
		{
			description: "All available messages should be passed to the RPC method then Finish",
			messages: []RPCMessage{
				newTestRPCMessage("Arith.Multiply", &Args{A: 2, B: 3}),
				nil,
			},
			timeout: 1 * time.Second,
			finish: func(t *testing.T, i *uint64) func(RPCMessage, RPCResult, error) {
				return func(m RPCMessage, r RPCResult, rpcErr error) {
					require.NoError(t, rpcErr, "Error from RPC method")
					assert.Equal(t, 6, *r.(*int), "Expected and actual results are different")
					atomic.AddUint64(i, 1)
				}
			},
		},
		{
			description: "Any error returned by the RPC method should be passed to Finish",
			messages: []RPCMessage{
				newTestRPCMessage("Arith.Multiply", &Args{Err: errors.New("test error")}),
				nil,
			},
			timeout: 1 * time.Second,
			finish: func(t *testing.T, i *uint64) func(RPCMessage, RPCResult, error) {
				return func(m RPCMessage, r RPCResult, rpcErr error) {
					assert.EqualError(
						t,
						rpcErr,
						"test error",
						"Expected and actual the RPC method errors are different")
					atomic.AddUint64(i, 1)
				}
			},
		},
		{
			description: "Errors caused by missing RPC methods should be passed to Finish",
			messages: []RPCMessage{
				newTestRPCMessage("Nope.Nope", &Args{A: 2, B: 3}),
				nil,
			},
			timeout: 1 * time.Second,
			finish: func(t *testing.T, i *uint64) func(RPCMessage, RPCResult, error) {
				return func(m RPCMessage, r RPCResult, rpcErr error) {
					assert.Error(t, rpcErr, "Expected error from missing RPC method call")
					atomic.AddUint64(i, 1)
				}
			},
		},
		{
			description: "Errors caused by wrong RPC method arguments type should be passed to Finish",
			messages:    []RPCMessage{newTestRPCMessage("Arith.Multiply", 4), nil},
			timeout:     1 * time.Second,
			finish: func(t *testing.T, i *uint64) func(RPCMessage, RPCResult, error) {
				return func(m RPCMessage, r RPCResult, rpcErr error) {
					assert.Error(t, rpcErr, "Expected error from missing RPC method call")
					atomic.AddUint64(i, 1)
				}
			},
		},
		{
			description: "Run should wait for all started goroutines to complete before returning",
			messages:    makeRPCMessages("Arith.Multiply", &Args{A: 2, B: 3}, 10000),
			timeout:     5 * time.Second,
			finish: func(t *testing.T, i *uint64) func(RPCMessage, RPCResult, error) {
				return func(m RPCMessage, r RPCResult, rpcErr error) {
					assert.Equal(t, 6, *r.(*int), "Expected and actual results are different")
					time.Sleep(10 * time.Millisecond)
					atomic.AddUint64(i, 1)
				}
			},
		},
		{
			description: "If the first message is nil, Run should exit immediately without calling the RPC method or Finish",
			messages:    []RPCMessage{nil},
			timeout:     1 * time.Second,
			finish:      func(*testing.T, *uint64) func(RPCMessage, RPCResult, error) { return nil },
		},
		{
			description: "If the shutdown timeout is reached, Run should return an error",
			messages:    []RPCMessage{newTestRPCMessage("Arith.Multiply", &Args{}), nil},
			timeout:     0,
			finish: func(*testing.T, *uint64) func(RPCMessage, RPCResult, error) {
				return func(RPCMessage, RPCResult, error) {
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

			server := rpc.NewServer()
			server.Register(new(Arith))

			var finishCount uint64
			svc := NewRPCService(testFinisher{
				finish: test.finish(t, &finishCount),
			})

			// Make a copy of the test.messages slice header so we don't modify the
			// test inputs. We're just re-slicing the messages slice to "pop" messages
			// off the top, so it won't modify the referenced RPCMessages.
			messages := test.messages
			err := svc.Run(func() RPCMessage {
				msg := messages[0]
				messages = messages[1:len(messages)]
				return msg
			}, server, test.timeout)
			require.Equal(t, test.wantErr, err, "Expected and actual errors are different")
			// The number of the RPC method and Finish calls is undefined when Run reaches
			// the shutdown timeout, so return if Run returns the timeout error.
			if err == errTimeout {
				return
			}
			assert.Equal(
				t,
				uint64(len(test.messages)-1),
				finishCount,
				"Expected and actual number of Finish calls are different")
		})
	}
}

func TestRPCService_HandleHTTP(t *testing.T) {
	// TODO: HandleHTTP tests.
}
