package node

import (
	"context"
	"encoding/hex"
	"math/rand"
	"os"

	"github.com/perlin-network/wavelet"
	"github.com/perlin-network/wavelet/log"
	"github.com/perlin-network/wavelet/params"
	"github.com/perlin-network/wavelet/security"

	"github.com/perlin-network/graph/database"
	"github.com/perlin-network/graph/graph"
	"github.com/perlin-network/graph/wire"
	"github.com/perlin-network/noise/dht"
	"github.com/perlin-network/noise/network"
	"github.com/perlin-network/noise/network/discovery"
	"github.com/perlin-network/noise/types/opcode"

	"github.com/perlin-network/wavelet/processor"
	"github.com/pkg/errors"
)

var _ network.PluginInterface = (*Wavelet)(nil)
var _ NodeInterface = (*Wavelet)(nil)
var PluginID = (*Wavelet)(nil)

type Options struct {
	DatabasePath  string
	GenesisPath   string
	ResetDatabase bool
}

type Wavelet struct {
	query
	broadcaster
	syncer

	net    *network.Network
	routes *dht.RoutingTable

	Ledger *wavelet.LoopHandle
	Wallet *wavelet.Wallet

	opts Options
}

const (
	WireTransactionOpcode           = 4000
	QueryResponseOpcode             = 4010
	SyncChildrenQueryRequestOpcode  = 4011
	SyncChildrenQueryResponseOpcode = 4012
	TxPushHintOpcode                = 4013
)

func NewPlugin(opts Options) *Wavelet {
	return &Wavelet{opts: opts}
}

func (w *Wavelet) Startup(net *network.Network) {
	w.net = net

	plugin, registered := net.Plugin(discovery.PluginID)

	if !registered {
		log.Fatal().Msg("Wavelet requires `discovery.Plugin` from the `noise` lib. to be registered into this nodes network.")
	}

	w.routes = plugin.(*discovery.Plugin).Routes

	if w.opts.ResetDatabase {
		err := os.RemoveAll(w.opts.DatabasePath)

		if err != nil {
			log.Info().Err(err).Str("db_path", w.opts.DatabasePath).Msg("Failed to delete previous database instance.")
		} else {
			log.Info().Str("db_path", w.opts.DatabasePath).Msg("Deleted previous database instance.")
		}
	}

	opcode.RegisterMessageType(opcode.Opcode(WireTransactionOpcode), &wire.Transaction{})
	opcode.RegisterMessageType(opcode.Opcode(QueryResponseOpcode), &QueryResponse{})
	opcode.RegisterMessageType(opcode.Opcode(SyncChildrenQueryRequestOpcode), &SyncChildrenQueryRequest{})
	opcode.RegisterMessageType(opcode.Opcode(SyncChildrenQueryResponseOpcode), &SyncChildrenQueryResponse{})
	opcode.RegisterMessageType(opcode.Opcode(TxPushHintOpcode), &TxPushHint{})

	ledger := wavelet.NewLedger(w.opts.DatabasePath, w.opts.GenesisPath)
	ledger.RegisterTransactionProcessor(params.TagNop, &processor.NopProcessor{})
	ledger.RegisterTransactionProcessor(params.TagGeneric, &processor.TransferProcessor{})
	ledger.RegisterTransactionProcessor(params.TagCreateContract, &processor.CreateContractProcessor{})
	ledger.RegisterTransactionProcessor(params.TagStake, &processor.StakeProcessor{})

	loop := wavelet.NewEventLoop(ledger)
	go loop.RunForever()

	w.Ledger = loop.Handle()

	w.Wallet = wavelet.NewWallet(net.GetKeys())

	w.query = query{Wavelet: w}
	w.query.sybil = stake{query: w.query}

	w.syncer = syncer{Wavelet: w}
	w.syncer.Start()

	w.broadcaster = broadcaster{Wavelet: w}
}

func (w *Wavelet) Receive(ctx *network.PluginContext) error {
	switch msg := ctx.Message().(type) {
	case *wire.Transaction:
		if validated, err := security.ValidateWiredTransaction(msg); err != nil || !validated {
			return errors.Wrap(err, "failed to validate incoming tx")
		}

		id := graph.Symbol(msg)

		res := &QueryResponse{Id: id}

		defer func() {
			err := ctx.Reply(context.Background(), res)
			if err != nil {
				log.Error().Err(err).Msg("Failed to send response.")
			}
		}()

		var existed bool

		w.LedgerDo(func(l wavelet.LedgerInterface) {
			existed = l.TransactionExists(id)
		})

		if existed {
			w.LedgerDo(func(l wavelet.LedgerInterface) {
				res.StronglyPreferred = l.IsStronglyPreferred(id)

				if res.StronglyPreferred && !l.WasAccepted(id) {
					err := l.QueueForAcceptance(id)

					if err != nil {
						log.Error().Err(err).Msg("Failed to queue transaction to pend for acceptance.")
					}
				}
			})

			log.Debug().Str("id", hex.EncodeToString(id)).Uint32("tag", msg.Tag).Msgf("Received an existing transaction, and voted '%t' for it.", res.StronglyPreferred)

			if rand.Intn(params.SyncNeighborsLikelihood) == 0 {
				go w.syncer.QueryMissingChildren(id)
				go w.syncer.QueryMissingParents(msg.Parents)
			}
		} else {
			go w.syncer.QueryMissingChildren(id)
			go w.syncer.QueryMissingParents(msg.Parents)

			var err error

			w.LedgerDo(func(l wavelet.LedgerInterface) {
				_, res.StronglyPreferred, err = l.RespondToQuery(msg)

				if err == nil && res.StronglyPreferred {
					err = l.QueueForAcceptance(id)
				}
			})

			if err != nil {
				if errors.Cause(err) == database.ErrTxExists {
					return nil
				}

				log.Warn().Err(err).Msg("Failed to respond to query or queue transaction to pend for acceptance.")
				return err
			}

			log.Debug().Str("id", hex.EncodeToString(id)).Uint32("tag", msg.Tag).Msgf("Received a new transaction, and voted '%t' for it.", res.StronglyPreferred)

			go func() {
				err := w.Query(msg)

				if err != nil {
					log.Debug().Err(err).Msg("Failed to gossip out transaction which was received.")
					return
				}

				var tx *database.Transaction

				w.LedgerDo(func(l wavelet.LedgerInterface) {
					tx, err = l.GetBySymbol(id)
				})

				if err != nil {
					log.Error().Err(err).Msg("Failed to find transaction which was received which was gossiped out.")
					return
				}

				w.LedgerDo(func(l wavelet.LedgerInterface) {
					err = l.HandleSuccessfulQuery(tx)
				})

				if err != nil {
					log.Error().Err(err).Msg("Failed to update conflict set for transaction received which was gossiped out.")
				}
			}()
		}
	case *SyncChildrenQueryRequest:
		var childrenIDs [][]byte

		w.LedgerDo(func(l wavelet.LedgerInterface) {
			if children, err := l.GetChildrenBySymbol(msg.Id); err == nil {
				childrenIDs = children.Transactions
			}
		})

		if childrenIDs == nil {
			childrenIDs = make([][]byte, 0)
		}

		ctx.Reply(context.Background(), &SyncChildrenQueryResponse{
			Children: childrenIDs,
		})
	case *TxPushHint:
		for _, id := range msg.Transactions {
			var out *wire.Transaction

			w.LedgerDo(func(l wavelet.LedgerInterface) {
				if tx, err := l.GetBySymbol(id); err == nil {
					out = &wire.Transaction{
						Sender:    tx.Sender,
						Nonce:     tx.Nonce,
						Parents:   tx.Parents,
						Tag:       tx.Tag,
						Payload:   tx.Payload,
						Signature: tx.Signature,
					}

					if out.Tag == params.TagCreateContract {
						// if it was a create contract that was removed from the db, load the tx payload from the ledger
						if len(out.Payload) == 0 {
							contractCode, err := l.LoadContract(id)
							if err != nil {
								return
							}
							out.Payload = contractCode
						}
					}
				}
			})

			if out != nil {
				w.net.BroadcastByAddresses(context.Background(), out, ctx.Sender().Address)
			}
		}
	}
	return nil
}

func (w *Wavelet) Cleanup(net *network.Network) {
	w.LedgerDo(func(l wavelet.LedgerInterface) {
		err := l.Cleanup()

		if err != nil {
			panic(err)
		}
	})
}

func (w *Wavelet) PeerConnect(client *network.PeerClient) {

}

func (w *Wavelet) PeerDisconnect(client *network.PeerClient) {
	log.Debug().Interface("ID", client.ID).Msgf("Peer disconnected: %s", client.Address)
}

func (w *Wavelet) LedgerDo(f func(ledger wavelet.LedgerInterface)) {
	w.Ledger.Do(func(l *wavelet.Ledger) {
		f(l)
	})
}
