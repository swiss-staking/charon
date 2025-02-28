// Copyright © 2022-2023 Obol Labs Inc. Licensed under the terms of a Business Source License 1.1

package leadercast

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/obolnetwork/charon/app/errors"
	"github.com/obolnetwork/charon/app/log"
	"github.com/obolnetwork/charon/app/z"
	"github.com/obolnetwork/charon/core"
	"github.com/obolnetwork/charon/p2p"
)

const protocol = "/charon/leadercast/1.0.0"

// Transport abstracts the transport layer for leader cast consensus.
type Transport interface {
	// Broadcast proposal all *other* peers.
	Broadcast(ctx context.Context, fromIdx int, d core.Duty, data core.UnsignedDataSet) error
	// AwaitNext blocks and returns when the next proposal is received from *another* peer.
	AwaitNext(ctx context.Context) (fromIdx int, d core.Duty, data core.UnsignedDataSet, err error)
}

func NewP2PTransport(tcpNode host.Host, peerIDx int, peers []peer.ID) Transport {
	t := &p2pTransport{
		tcpNode: tcpNode,
		peers:   peers,
		peerIDx: peerIDx,
		ch:      make(chan p2pMsg),
	}
	t.tcpNode.SetStreamHandler(protocol, t.handle)

	return t
}

type p2pTransport struct {
	tcpNode host.Host
	peerIDx int
	peers   []peer.ID
	ch      chan p2pMsg
}

// handle implements p2p network.StreamHandler processing new incoming messages.
func (t *p2pTransport) handle(s network.Stream) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	ctx = log.WithCtx(ctx, z.Str("peer", p2p.PeerName(s.Conn().RemotePeer())))

	defer cancel()
	defer s.Close()

	var msg p2pMsg
	err := json.NewDecoder(s).Decode(&msg)
	if err != nil {
		log.Error(ctx, "Decode leadercast message", err)
		return
	}
	select {
	case t.ch <- msg:
	case <-ctx.Done():
		log.Warn(ctx, "Leadercast transport buffer full", nil)
	}
}

func (t *p2pTransport) Broadcast(ctx context.Context, fromIdx int, d core.Duty, data core.UnsignedDataSet) error {
	b, err := json.Marshal(p2pMsg{
		FromIdx: fromIdx,
		Duty:    d,
		Data:    data,
	})
	if err != nil {
		return errors.Wrap(err, "marshal tcpNode msg")
	}

	var errs []error

	for idx, p := range t.peers {
		if idx == t.peerIDx {
			// Don't send to self.
			continue
		}

		err := sendData(ctx, t, p, b)
		if err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) == 0 {
		log.Debug(ctx, "Leader propose value success", z.Any("duty", d))
	} else {
		// len(t.peers)-len(errs)-1 is total number of errors excluding broadcast to self case
		log.Warn(ctx, "Leader propose value with errors", errs[0], z.Int("success", len(t.peers)-len(errs)-1),
			z.Int("errors", len(errs)))
	}

	return nil
}

func (t *p2pTransport) AwaitNext(ctx context.Context) (int, core.Duty, core.UnsignedDataSet, error) {
	select {
	case <-ctx.Done():
		return 0, core.Duty{}, nil, ctx.Err()
	case msg := <-t.ch:
		return msg.FromIdx, msg.Duty, msg.Data, nil
	}
}

func sendData(ctx context.Context, t *p2pTransport, p peer.ID, b []byte) error {
	// Circuit relay connections are transient
	s, err := t.tcpNode.NewStream(network.WithUseTransient(ctx, "leadercast"), p, protocol)
	if err != nil {
		return errors.Wrap(err, "tcpNode stream")
	}

	_, err = s.Write(b)
	if err != nil {
		return errors.Wrap(err, "tcpNode write")
	}

	if err := s.Close(); err != nil {
		return errors.Wrap(err, "tcpNode close")
	}

	return nil
}

type p2pMsg struct {
	Idx     int
	FromIdx int
	Duty    core.Duty
	Data    core.UnsignedDataSet
}

func (m p2pMsg) MarshalJSON() ([]byte, error) {
	data := make(map[core.PubKey]json.RawMessage)
	for key, val := range m.Data {
		b, err := val.MarshalJSON()
		if err != nil {
			return nil, errors.Wrap(err, "marshal unsigned data")
		}
		data[key] = b
	}

	b, err := json.Marshal(p2pMsgJSON{
		Idx:     m.Idx,
		FromIdx: m.FromIdx,
		Duty:    m.Duty,
		Data:    data,
	})
	if err != nil {
		return nil, errors.Wrap(err, "marshal p2p msg")
	}

	return b, nil
}

func (m *p2pMsg) UnmarshalJSON(input []byte) error {
	var msg p2pMsgJSON
	if err := json.Unmarshal(input, &msg); err != nil {
		return errors.Wrap(err, "unmarshal p2p msg")
	}

	set := make(core.UnsignedDataSet)
	for key, val := range msg.Data {
		data, err := core.UnmarshalUnsignedData(msg.Duty.Type, val)
		if err != nil {
			return errors.Wrap(err, "marshal unsigned data")
		}
		set[key] = data
	}

	*m = p2pMsg{
		Idx:     msg.Idx,
		FromIdx: msg.FromIdx,
		Duty:    msg.Duty,
		Data:    set,
	}

	return nil
}

type p2pMsgJSON struct {
	Idx     int
	FromIdx int
	Duty    core.Duty
	Data    map[core.PubKey]json.RawMessage
}

// NewMemTransportFunc returns a function that itself returns in-memory
// transport instances that communicate with each other.
// It stops processing messages when the context is closed.
func NewMemTransportFunc(ctx context.Context) func() Transport {
	var (
		input   = make(chan p2pMsg)
		outputs = make(map[int]chan p2pMsg)
		mu      sync.Mutex
	)

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-input:
				mu.Lock()
				for idx, output := range outputs {
					if msg.Idx == idx {
						continue
					}
					output <- msg
				}
				mu.Unlock()
			}
		}
	}()

	var index int

	return func() Transport {
		mu.Lock()
		defer mu.Unlock()

		idx := index
		output := make(chan p2pMsg)
		outputs[idx] = output

		index++

		return memTransport{
			idx:    idx,
			input:  input,
			output: output,
		}
	}
}

// memTransport is an in-memory transport useful for deterministic integration tests.
type memTransport struct {
	idx    int
	input  chan<- p2pMsg
	output <-chan p2pMsg
}

func (m memTransport) Broadcast(ctx context.Context, fromIdx int, duty core.Duty, data core.UnsignedDataSet) error {
	msg := p2pMsg{
		Idx:     m.idx,
		FromIdx: fromIdx,
		Duty:    duty,
		Data:    data,
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case m.input <- msg:
		return nil
	}
}

func (m memTransport) AwaitNext(ctx context.Context) (int, core.Duty, core.UnsignedDataSet, error) {
	for {
		select {
		case <-ctx.Done():
			return 0, core.Duty{}, nil, ctx.Err()
		case msg := <-m.output:
			return msg.FromIdx, msg.Duty, msg.Data, nil
		}
	}
}
