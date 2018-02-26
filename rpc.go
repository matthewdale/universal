package universal

import (
	"context"
	"fmt"
	"net/rpc"
	"reflect"
	"sync"
	"time"

	"github.com/pkg/errors"
)

type RPCMessage interface {
	Context() context.Context
	Request() rpc.Request
	Args() interface{}
}

// An RPCResult is the result value from an RPC method call. The caller is
// responsible for type-asserting the value to the type of the result of the
// called RPC method (the 2nd argument of the RPC method).
type RPCResult = interface{}

type RPCFinisher interface {
	Finish(RPCMessage, RPCResult, error)
}

type RPCService struct {
	finisher RPCFinisher
	wg       sync.WaitGroup
}

func NewRPCService(finisher RPCFinisher) *RPCService {
	return &RPCService{finisher: finisher}
}

// Run calls the specified RPC method then Finish on all messages returned by
// the provided RPCMessage source function, blocking until the provided RPCMessage
// source function returns nil.
//
// When the provided RPCMessage source returns a nil RPCMessage, Run starts
// to shut down, blocking until all running goroutines started by Run have
// returned or until the shutdownTimeout is reached, whichever comes first. If
// the shutdownTimeout is reached, Run returns an error.
func (svc *RPCService) Run(
	server *rpc.Server,
	next func() RPCMessage,
	shutdownTimeout time.Duration,
) error {
	for {
		msg := next()
		if msg == nil {
			break
		}
		svc.wg.Add(1)
		go svc.serveAndFinish(server, msg)
	}
	// Wait until the WaitGroup is done, indicating that all started goroutines
	// have returned, or until the shutdownTimeout duration has been reached.
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

func (svc *RPCService) serveAndFinish(server *rpc.Server, msg RPCMessage) {
	defer svc.wg.Done()
	codec := &rpcMessageCodec{message: msg}
	err := server.ServeRequest(codec)
	if err != nil {
		svc.finisher.Finish(msg, nil, err)
		return
	}
	var respErr error
	if codec.response.Error != "" {
		respErr = errors.New(codec.response.Error)
	}
	svc.finisher.Finish(msg, codec.result, respErr)
}

// rpcMessageCodec implements the rpc.ServerCodec interface, sourcing the inputs
// for an rpc.Server.ServeRequest call from the rpc.Request and arguments provided
// by the RPCMessage. It also collects the returned rpc.Response and result.
type rpcMessageCodec struct {
	message  RPCMessage
	response rpc.Response
	result   RPCResult
}

func (codec *rpcMessageCodec) ReadRequestHeader(r *rpc.Request) error {
	*r = codec.message.Request()
	return nil
}

// ReadRequestBody shallow-copies the RPC arguments provided by the RPCMessage
// into the body arg, which is an output arg. The body arg must be a pointer.
func (codec *rpcMessageCodec) ReadRequestBody(body interface{}) error {
	if body == nil {
		return nil
	}
	// The body arg is a pointer, so we must use reflection to dereference the
	// pointer and copy in the RPC arguments.
	vDst := reflect.Indirect(reflect.ValueOf(body))
	// The RPCMessage constructor expects the args type to match the target
	// RPC method argument type exactly, meaning it should be a pointer. Use
	// reflect.Indirect to dereference the request body so the types are comparable.
	// In the case that args is not a pointer, reflect.Indirect will return the
	// original value, so the operation is safe in either case.
	vSrc := reflect.Indirect(reflect.ValueOf(codec.message.Args()))
	if vDst.Type() != vSrc.Type() {
		return fmt.Errorf(
			"input body and output body are different types (input is %s, output is %s)",
			vSrc.Type().String(),
			vDst.Type().String())
	}
	// Calling Set on an unassignable reflect.Value panics. However, that can only
	// happen if the input body is not a pointer, in which case the Go net/rpc
	// implementation is completely broken for all users, so a panic is appropriate.
	vDst.Set(vSrc)
	return nil
}

func (codec *rpcMessageCodec) WriteResponse(r *rpc.Response, body interface{}) error {
	if r != nil {
		codec.response = *r
	}
	codec.result = body
	return nil
}

// Close is a no-op and exists only to satisfy the rpc.ServerCodec interface.
func (codec *rpcMessageCodec) Close() error { return nil }
