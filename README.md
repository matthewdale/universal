# universal [![Build Status](https://travis-ci.org/matthewdale/universal.svg?branch=master)](https://travis-ci.org/matthewdale/universal) [![codecov](https://codecov.io/gh/matthewdale/universal/branch/master/graph/badge.svg)](https://codecov.io/gh/matthewdale/universal) [![Go Report Card](https://goreportcard.com/badge/github.com/matthewdale/universal)](https://goreportcard.com/report/github.com/matthewdale/universal) [![GoDoc](https://godoc.org/github.com/matthewdale/universal?status.svg)](https://godoc.org/github.com/matthewdale/universal)

Package universal provides interfaces for expressing message processing logic and utilities that simplify building and running message processing applications. It requires Go 1.9 or newer.

There are many different ways to build a message processing application with Go. However, the basic requirements of those applications are typically the same:

1.  It must accept an incoming message from a message source (e.g. a message queue or database).
2.  It must perform work on the incoming message and write a result.
3.  It must persist the result in a message destination (e.g. another message queue or database).

An additional practical requirement is that the message processing logic must be callable on-demand to provide visibility into the logic (e.g. for troubleshooting unexpected message processing results).

# Simple message processor

A `Service` receives messages from the provided message source and calls the `Processor` functions for every message in separate goroutines. The `Service` can also be used as an `http.Handler`, which calls the `Processor` using the HTTP request body as the message body and returns the results in the response body.

## Example
```go
type MyMessage struct {
	ctx    context.Context
	input  *bytes.Reader
	result *bytes.Buffer
}

func (msg *MyMessage) Context() context.Context    { return msg.ctx }
func (msg *MyMessage) Read(p []byte) (int, error)  { return msg.input.Read(p) }
func (msg *MyMessage) Write(p []byte) (int, error) { return msg.result.Write(p) }

type MyProcessor struct{}

func (proc *MyProcessor) Process(m universal.Message) error {
	// Read the message, perform work, and write the result.
	_, err = m.Write([]byte("result"))
	return err
}

func (proc *MyProcessor) Finish(m universal.Message, procErr error) {
	if procErr != nil {
		// Handle any error returned from Process.
		return
	}
	message := m.(*MyMessage)
	MessageSource.SendMessage(message.result.Bytes())
}

func main() {
	svc := universal.NewService(&MyProcessor{})
	go svc.Run(func() universal.Message {
		// Get the next message from a message source.
		input := MessageSource.GetMessage()
		return &MyMessage{
			ctx:    context.Background(),
			input:  bytes.NewReader(input.Body),
			result: new(bytes.Buffer),
		}
	}, 30 * time.Second)
	http.ListenAndServe(":80", svc)
}
```

# RPC message processor

An `RPCService` receives messages from the provided message source and calls an existing RPC server, passing the RPC method call results to the `RPCFinisher` function.

## Example
```go
type Args struct {
	A, B int
}

type Arith int

func (t *Arith) Multiply(args *Args, reply *int) error {
	*reply = args.A * args.B
	return nil
}

type MyRPCMessage struct {
	ctx     context.Context
	request rpc.Request
	args    interface{}
}

func (rw *MyRPCMessage) Context() context.Context { return rw.ctx }
func (rw *MyRPCMessage) Request() rpc.Request     { return rw.request }
func (rw *MyRPCMessage) Args() interface{}        { return rw.args }

type MyRPCFinisher struct{}

func (rw *MyRPCFinisher) Finish(
	m universal.RPCMessage,
	r universal.RPCResult,
	rpcErr error,
) {
	if rpcErr != nil {
		// Handle any error returned from the RPC method.
		return
    }
    result := r.(*int)
	MessageSource.SendMessage([]byte(strconv.Itoa(result)))
}

func main() {
	server := rpc.NewServer()
	server.Register(new(Arith))

	svc := universal.NewRPCService(&MyRPCFinisher{})
	go svc.Run(server, func() universal.RPCMessage {
		// Get the next message from a message source.
		input := MessageSource.GetMessage()
		return &MyRPCMessage{
			ctx:     context.Background(),
			request: rpc.Request{ServiceMethod: input.Method},
			args: &Args{
				A: input.A,
				B: input.B,
			},
		}
	}, 30 * time.Second)

	server.HandleHTTP(rpc.DefaultRPCPath, rpc.DefaultDebugPath)
	http.ListenAndServe(":80", nil)
}
```
