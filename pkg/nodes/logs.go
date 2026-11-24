package nodes

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/ponyo877/agones-lite/pkg/agent"
	"github.com/ponyo877/agones-lite/pkg/store"
	nodev1 "github.com/ponyo877/agones-lite/proto/node/v1"
)

// logChunk is how much of a log travels in one message.
const logChunk = 32 * 1024

// LogReader reads game server logs on one node. *agent.Agent is one.
type LogReader interface {
	Logs(ctx context.Context, req agent.LogRequest) (io.ReadCloser, error)
}

// Attach says who reads the logs on a node: the agent itself when it runs
// in this process, a stand-in that asks it over the network otherwise.
func (l *Local) Attach(node string, reader LogReader) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.readers[node] = reader
}

// Detach forgets reader when the agent it reads through went away. An agent
// that dials again may attach its next reader before the old one is seen
// to be gone, and that next one has to stay.
func (l *Local) Detach(node string, reader LogReader) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.readers[node] == reader {
		delete(l.readers, node)
	}
}

// Logs reads a game server's log from whichever node it runs on.
func (l *Local) Logs(ctx context.Context, req agent.LogRequest) (io.ReadCloser, error) {
	gs, ok := l.servers.Get(req.Namespace, req.Name)
	if !ok {
		return nil, store.ErrNotFound
	}
	l.mu.Lock()
	reader, connected := l.readers[gs.Status.NodeName]
	l.mu.Unlock()
	if !connected {
		return nil, fmt.Errorf("gameserver %s/%s: its node %q is not connected: %w", req.Namespace, req.Name, gs.Status.NodeName, agent.ErrNoLog)
	}
	return reader.Logs(ctx, req)
}

// remoteLogs reads logs on a node whose agent is on another machine. Only
// the agent can dial, so the request rides on the agent's open Watch call
// and the agent answers by calling Logs.
type remoteLogs struct {
	server   *Server
	requests chan *nodev1.LogRequest // picked up by the node's Watch call
}

func (r *remoteLogs) Logs(ctx context.Context, req agent.LogRequest) (io.ReadCloser, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}
	reader, writer := io.Pipe()
	r.server.expect(id, writer)
	request := &nodev1.LogRequest{
		Id: id, Namespace: req.Namespace, Name: req.Name,
		Container: req.Container, Follow: req.Follow, Tail: int32(req.Tail),
	}
	select {
	case r.requests <- request:
		return &pendingLog{PipeReader: reader, forget: func() { r.server.forget(id) }}, nil
	case <-ctx.Done():
		r.server.forget(id)
		return nil, ctx.Err()
	}
}

// pendingLog is the reading end of a log on its way from an agent. Closing
// it tells the control plane to stop accepting that log.
type pendingLog struct {
	*io.PipeReader
	forget func()
}

func (p *pendingLog) Close() error {
	p.forget()
	return p.PipeReader.Close()
}

func (s *Server) expect(id string, writer *io.PipeWriter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logs[id] = writer
}

func (s *Server) forget(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.logs, id)
}

// Logs implements the Logs call of the protocol: an agent delivering a log
// that was asked for.
func (s *Server) Logs(stream nodev1.Nodes_LogsServer) error {
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return stream.SendAndClose(&nodev1.LogsResponse{})
		}
		if err != nil {
			return err
		}
		if err := s.deliver(chunk); err != nil {
			return err // the reader is gone; the agent stops sending
		}
	}
}

// deliver hands one chunk to whoever asked for the log.
func (s *Server) deliver(chunk *nodev1.LogChunk) error {
	s.mu.Lock()
	writer, ok := s.logs[chunk.GetId()]
	s.mu.Unlock()
	if !ok {
		return errors.New("nobody is waiting for this log")
	}
	if chunk.GetError() != "" {
		return writer.CloseWithError(errors.New(chunk.GetError()))
	}
	if len(chunk.GetData()) == 0 {
		return writer.Close() // the agent's way of saying "that was all"
	}
	_, err := writer.Write(chunk.GetData())
	return err
}

func newID() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// Serve sets who reads logs on this agent's node. Call it before Run.
func (r *Remote) Serve(reader LogReader) {
	r.logs = reader
}

// sendLog answers a LogRequest: it reads the log and streams it back.
func (r *Remote) sendLog(ctx context.Context, req *nodev1.LogRequest) {
	stream, err := r.client.Logs(ctx)
	if err != nil {
		return
	}
	defer func() { _, _ = stream.CloseAndRecv() }()
	log, err := r.logs.Logs(ctx, agent.LogRequest{
		Namespace: req.GetNamespace(), Name: req.GetName(),
		Container: req.GetContainer(), Follow: req.GetFollow(), Tail: int(req.GetTail()),
	})
	if err != nil {
		_ = stream.Send(&nodev1.LogChunk{Id: req.GetId(), Error: err.Error()})
		return
	}
	defer func() { _ = log.Close() }()
	buffer := make([]byte, logChunk)
	for {
		n, err := log.Read(buffer)
		if n > 0 && stream.Send(&nodev1.LogChunk{Id: req.GetId(), Data: buffer[:n]}) != nil {
			return // the control plane stopped listening
		}
		if err != nil {
			_ = stream.Send(&nodev1.LogChunk{Id: req.GetId()}) // an empty chunk ends the log
			return
		}
	}
}
