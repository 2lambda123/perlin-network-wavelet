package node

import (
	"github.com/perlin-network/noise"
	"github.com/perlin-network/noise/payload"
	"github.com/perlin-network/wavelet"
	"github.com/perlin-network/wavelet/common"
	"github.com/pkg/errors"
)

var (
	_ noise.Message = (*GossipRequest)(nil)
	_ noise.Message = (*GossipResponse)(nil)
	_ noise.Message = (*QueryRequest)(nil)
	_ noise.Message = (*QueryResponse)(nil)
	_ noise.Message = (*SyncViewRequest)(nil)
	_ noise.Message = (*SyncViewResponse)(nil)
)

type QueryRequest struct {
	tx *wavelet.Transaction
}

func (q QueryRequest) Read(reader payload.Reader) (noise.Message, error) {
	msg, err := wavelet.Transaction{}.Read(reader)
	if err != nil {
		return nil, errors.Wrap(err, "wavelet: failed to read query request tx")
	}

	tx := msg.(wavelet.Transaction)
	q.tx = &tx

	return q, nil
}

func (q QueryRequest) Write() []byte {
	return q.tx.Write()
}

type QueryResponse struct {
	preferred common.TransactionID
}

func (q QueryResponse) Read(reader payload.Reader) (noise.Message, error) {
	n, err := reader.Read(q.preferred[:])

	if err != nil {
		return nil, errors.Wrap(err, "wavelet: failed to read query response preferred id")
	}

	if n != len(q.preferred) {
		return nil, errors.New("wavelet: didn't read enough bytes for query response preferred id")
	}

	return q, nil
}

func (q QueryResponse) Write() []byte {
	return q.preferred[:]
}

type GossipRequest struct {
	tx *wavelet.Transaction
}

func (q GossipRequest) Read(reader payload.Reader) (noise.Message, error) {
	msg, err := wavelet.Transaction{}.Read(reader)
	if err != nil {
		return nil, errors.Wrap(err, "wavelet: failed to read gossip request tx")
	}

	tx := msg.(wavelet.Transaction)
	q.tx = &tx

	return q, nil
}

func (q GossipRequest) Write() []byte {
	return q.tx.Write()
}

type GossipResponse struct {
	vote bool
}

func (q GossipResponse) Read(reader payload.Reader) (noise.Message, error) {
	vote, err := reader.ReadByte()
	if err != nil {
		return nil, errors.Wrap(err, "wavelet: failed to read gossip response vote")
	}

	if vote == 1 {
		q.vote = true
	}

	return q, nil
}

func (q GossipResponse) Write() []byte {
	writer := payload.NewWriter(nil)

	if q.vote {
		writer.WriteByte(1)
	} else {
		writer.WriteByte(0)
	}

	return writer.Bytes()
}

type SyncViewRequest struct {
	viewID uint64
}

func (s SyncViewRequest) Read(reader payload.Reader) (noise.Message, error) {
	var err error

	s.viewID, err = reader.ReadUint64()
	if err != nil {
		return nil, errors.Wrap(err, "failed to read view ID")
	}

	return s, nil
}

func (s SyncViewRequest) Write() []byte {
	return payload.NewWriter(nil).WriteUint64(s.viewID).Bytes()
}

type SyncViewResponse struct {
	root *wavelet.Transaction
}

func (s SyncViewResponse) Read(reader payload.Reader) (noise.Message, error) {
	msg, err := wavelet.Transaction{}.Read(reader)
	if err != nil {
		return nil, errors.Wrap(err, "failed to read root tx")
	}

	root := msg.(wavelet.Transaction)
	s.root = &root

	return s, nil
}

func (s SyncViewResponse) Write() []byte {
	return s.root.Write()
}
