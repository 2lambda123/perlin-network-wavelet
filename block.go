package wavelet

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/pkg/errors"
	"golang.org/x/crypto/blake2b"
)

type Block struct {
	Index        uint64
	Merkle       MerkleNodeID
	Transactions []TransactionID

	ID BlockID
}

func NewBlock(index uint64, merkle MerkleNodeID, ids ...TransactionID) Block {
	b := Block{Index: index, Merkle: merkle, Transactions: ids}

	buf := bytes.NewBuffer(nil)
	for _, id := range ids {
		buf.Write(id[:])
	}
	b.ID = blake2b.Sum256(buf.Bytes())

	return b
}

func (b *Block) GetID() string {
	if b == nil || b.ID == ZeroBlockID {
		return ""
	}

	return fmt.Sprintf("%x", b.ID)
}

func (b Block) Marshal() []byte {
	buf := bytes.NewBuffer(make([]byte, 0, 8+SizeMerkleNodeID+4+4+len(b.Transactions)*SizeTransactionID))

	binary.Write(buf, binary.BigEndian, b.Index)
	buf.Write(b.Merkle[:])
	binary.Write(buf, binary.BigEndian, uint32(len(b.Transactions)))

	for _, id := range b.Transactions {
		buf.Write(id[:])
	}

	return buf.Bytes()
}

func (b Block) String() string {
	return hex.EncodeToString(b.ID[:])
}

func UnmarshalBlock(r io.Reader) (block Block, err error) {
	var buf [8]byte

	if _, err = io.ReadFull(r, buf[:]); err != nil {
		err = errors.Wrap(err, "failed to decode block index")
		return
	}
	block.Index = binary.BigEndian.Uint64(buf[:8])

	if _, err = io.ReadFull(r, block.Merkle[:]); err != nil {
		err = errors.Wrap(err, "failed to decode block's merkle root")
		return
	}

	if _, err = io.ReadFull(r, buf[:4]); err != nil {
		err = errors.Wrap(err, "failed to decode block's transactions length")
		return
	}

	block.Transactions = make([]TransactionID, binary.BigEndian.Uint32(buf[:4]))

	for i := 0; i < len(block.Transactions); i++ {
		if _, err = io.ReadFull(r, block.Transactions[i][:]); err != nil {
			err = errors.Wrap(err, "failed to decode one of the transactions")
			return
		}
	}
	
	idBuf := bytes.NewBuffer(nil)
	for _, id := range block.Transactions {
		idBuf.Write(id[:])
	}
	block.ID = blake2b.Sum256(idBuf.Bytes())

	return
}
