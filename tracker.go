package tracker

import (
	"context"
	"fmt"
	"sync"

	"github.com/apache/thrift/lib/go/thrift"
	"github.com/damnever/tracker/tracking"
)

type ctxKey string

const (
	CtxKeyRequestID              ctxKey = "__thrift_tracking_request_id"
	CtxKeyRequestMeta            ctxKey = "__thrift_tracking_request_meta"
	CtxKeyResponseMeta           ctxKey = "__thrift_tracking_response_meta"
	TrackingAPIName              string = "ElemeThriftTrackingAPI"
	VersionDefault               int32  = 0
	VersionRequestHeader         int32  = 1
	VersionRequestResponseHeader int32  = 2
	VersionMax                   int32  = VersionRequestResponseHeader
)

type HandShaker interface {
	Negotiation(curSeqID int32, iprot, oprot thrift.TProtocol) error
	TryUpgrade(seqID int32, iprot, oprot thrift.TProtocol) (bool, thrift.TException)
	RequestHeaderSupported() bool
	ResponseHeaderSupported() bool
}

type Tracker interface {
	HandShaker

	RequestID(ctx context.Context) string
	TryReadRequestHeader(iprot thrift.TProtocol) error
	TryWriteRequestHeader(ctx context.Context, oprot thrift.TProtocol) error
	TryReadResponseHeader(iprot thrift.TProtocol) error
	TryWriteResponseHeader(ctx context.Context, oprot thrift.TProtocol) error
}

type NewTrackerFactoryFunc func() Tracker

type Hooks struct {
	onHandshakRequest func(args *tracking.UpgradeArgs_)
	onHeaderRequest   func(header *tracking.RequestHeader)
	onHeaderResponse  func(header *tracking.ResponseHeader)
}

var DefaultHooks = Hooks{
	onHandshakRequest: func(args *tracking.UpgradeArgs_) { fmt.Printf("%#+v\n", args) },
	onHeaderRequest:   func(header *tracking.RequestHeader) { fmt.Printf("%#+v\n", header) },
	onHeaderResponse:  func(header *tracking.ResponseHeader) { fmt.Printf("%#+v\n", header) },
}

type SimpleTracker struct {
	mu      *sync.RWMutex
	version int32
	client  string
	server  string
	hooks   Hooks
}

func NewSimpleTrackerFactory(client, server string, hooks Hooks) func() Tracker {
	return func() Tracker {
		return NewSimpleTracker(client, server, hooks)
	}
}

func NewSimpleTracker(client, server string, hooks Hooks) Tracker {
	return &SimpleTracker{
		mu:      &sync.RWMutex{},
		version: VersionDefault,
		client:  client,
		server:  server,
		hooks:   hooks,
	}
}

func (t *SimpleTracker) Negotiation(curSeqID int32, iprot, oprot thrift.TProtocol) error {
	// send
	if err := oprot.WriteMessageBegin(TrackingAPIName, thrift.CALL, curSeqID); err != nil {
		return err
	}
	args := tracking.NewUpgradeArgs_()
	args.AppID = t.client
	args.Version = t.version
	if err := args.Write(oprot); err != nil {
		return err
	}
	if err := oprot.WriteMessageEnd(); err != nil {
		return err
	}
	if err := oprot.Flush(); err != nil {
		return err
	}

	// recv
	method, mTypeID, seqID, err := iprot.ReadMessageBegin()
	if err != nil {
		return err
	}
	if method != TrackingAPIName {
		return thrift.NewTApplicationException(thrift.WRONG_METHOD_NAME,
			"tracker negotiation failed: wrong method name")
	}
	if curSeqID != seqID {
		return thrift.NewTApplicationException(thrift.BAD_SEQUENCE_ID,
			"tracker negotiation failed: out of sequence response")
	}
	if mTypeID == thrift.EXCEPTION {
		err0 := thrift.NewTApplicationException(thrift.UNKNOWN_APPLICATION_EXCEPTION,
			"Unknown Exception")
		var err1, err error
		if err1, err = err0.Read(iprot); err != nil {
			return err
		}
		if err = iprot.ReadMessageEnd(); err != nil {
			return err
		}
		return err1
	}
	if mTypeID != thrift.REPLY {
		return thrift.NewTApplicationException(thrift.INVALID_MESSAGE_TYPE_EXCEPTION,
			"tracker negotiation failed: invalid message type")
	}
	reply := tracking.NewUpgradeReply()
	if err := reply.Read(iprot); err != nil {
		return err
	}
	if err := iprot.ReadMessageEnd(); err != nil {
		return err
	}
	t.trySetVersion(reply.GetVersion(), VersionRequestHeader)
	return nil
}

func (t *SimpleTracker) TryUpgrade(seqID int32, iprot, oprot thrift.TProtocol) (bool, thrift.TException) {
	args := tracking.NewUpgradeArgs_()
	if err := args.Read(iprot); err != nil {
		iprot.ReadMessageEnd()
		x := thrift.NewTApplicationException(thrift.PROTOCOL_ERROR, err.Error())
		x.Write(oprot)
		oprot.WriteMessageEnd()
		oprot.Flush()
		return false, err
	}
	iprot.ReadMessageEnd()

	t.hooks.onHandshakRequest(args)
	result := tracking.NewUpgradeReply()
	version := args.GetVersion()
	if version > VersionDefault {
		t.trySetVersion(version, version)
		result.Version = version
	} else {
		t.trySetVersion(version, VersionRequestHeader)
	}
	if err := oprot.WriteMessageBegin(TrackingAPIName, thrift.REPLY, seqID); err != nil {
		return false, err
	}
	if err := result.Write(oprot); err != nil {
		return false, err
	}
	if err := oprot.WriteMessageEnd(); err != nil {
		return false, err
	}
	if err := oprot.Flush(); err != nil {
		return false, err
	}
	return true, nil
}

func (t *SimpleTracker) trySetVersion(version int32, defaultVersion int32) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if version == VersionDefault {
		version = defaultVersion
	}
	if version < t.version || version > VersionMax {
		return
	}
	t.version = version
}

func (t *SimpleTracker) RequestHeaderSupported() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.version >= VersionRequestHeader
}

func (t *SimpleTracker) ResponseHeaderSupported() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.version >= VersionRequestResponseHeader
}

func (t *SimpleTracker) RequestID(ctx context.Context) string {
	if reqID, ok := ctx.Value(CtxKeyRequestID).(string); ok {
		return reqID
	}
	return "TODO"
}

func (t *SimpleTracker) TryReadRequestHeader(iprot thrift.TProtocol) error {
	if !t.RequestHeaderSupported() {
		return nil
	}
	header := tracking.NewRequestHeader()
	if err := header.Read(iprot); err != nil {
		return err
	}
	t.hooks.onHeaderRequest(header)
	return nil
}

func (t *SimpleTracker) TryWriteRequestHeader(ctx context.Context, oprot thrift.TProtocol) error {
	if !t.RequestHeaderSupported() {
		return nil
	}
	header := tracking.NewRequestHeader()
	if meta, ok := ctx.Value(CtxKeyRequestMeta).(map[string]string); ok {
		header.Meta = meta
	}
	header.RequestID = t.RequestID(ctx) // TODO
	header.Seq = "1.2"                  // TODO
	return header.Write(oprot)
}

func (t *SimpleTracker) TryReadResponseHeader(iprot thrift.TProtocol) error {
	if !t.ResponseHeaderSupported() {
		return nil
	}
	header := tracking.NewResponseHeader()
	if err := header.Read(iprot); err != nil {
		return err
	}
	t.hooks.onHeaderResponse(header)
	return nil
}

func (t *SimpleTracker) TryWriteResponseHeader(ctx context.Context, oprot thrift.TProtocol) error {
	if !t.ResponseHeaderSupported() {
		return nil
	}
	header := tracking.NewResponseHeader()
	if meta, ok := ctx.Value(CtxKeyResponseMeta).(map[string]string); ok {
		header.Meta = meta
	}
	return header.Write(oprot)
}
