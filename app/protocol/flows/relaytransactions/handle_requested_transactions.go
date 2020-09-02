package relaytransactions

import (
	"github.com/kaspanet/kaspad/app/appmessage"
	"github.com/kaspanet/kaspad/infrastructure/network/netadapter/router"
)

type handleRequestedTransactionsFlow struct {
	TransactionsRelayContext
	incomingRoute, outgoingRoute *router.Route
}

// HandleRequestedTransactions listens to appmessage.MsgRequestTransactions messages, responding with the requested
// transactions if those are in the mempool.
// Missing transactions would be ignored
func HandleRequestedTransactions(context TransactionsRelayContext, incomingRoute *router.Route, outgoingRoute *router.Route) error {
	flow := &handleRequestedTransactionsFlow{
		TransactionsRelayContext: context,
		incomingRoute:            incomingRoute,
		outgoingRoute:            outgoingRoute,
	}
	return flow.start()
}

func (flow *handleRequestedTransactionsFlow) start() error {
	for {
		msgRequestTransactions, err := flow.readRequestTransactions()
		if err != nil {
			return err
		}

		for _, transactionID := range msgRequestTransactions.IDs {
			log.Criticalf("~~~~~ handleRequestedTransactionsFlow.start() tx %s was requested", transactionID)
			tx, ok := flow.TxPool().FetchTransaction(transactionID)

			if !ok {
				msgTransactionNotFound := appmessage.NewMsgTransactionNotFound(transactionID)
				err := flow.outgoingRoute.Enqueue(msgTransactionNotFound)
				if err != nil {
					return err
				}
				continue
			}

			err := flow.outgoingRoute.Enqueue(tx.MsgTx())
			if err != nil {
				return err
			}
		}
	}
}

func (flow *handleRequestedTransactionsFlow) readRequestTransactions() (*appmessage.MsgRequestTransactions, error) {
	msg, err := flow.incomingRoute.Dequeue()
	if err != nil {
		return nil, err
	}

	return msg.(*appmessage.MsgRequestTransactions), nil
}
