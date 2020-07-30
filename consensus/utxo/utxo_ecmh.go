package utxo

import (
	"bytes"
	"github.com/kaspanet/go-secp256k1"
	"github.com/kaspanet/kaspad/wire"
)

func AddUTXOToMultiset(ms *secp256k1.MultiSet, entry *UTXOEntry, outpoint *wire.Outpoint) (*secp256k1.MultiSet, error) {
	w := &bytes.Buffer{}
	err := SerializeUTXO(w, entry, outpoint)
	if err != nil {
		return nil, err
	}
	ms.Add(w.Bytes())
	return ms, nil
}

func RemoveUTXOFromMultiset(ms *secp256k1.MultiSet, entry *UTXOEntry, outpoint *wire.Outpoint) (*secp256k1.MultiSet, error) {
	w := &bytes.Buffer{}
	err := SerializeUTXO(w, entry, outpoint)
	if err != nil {
		return nil, err
	}
	ms.Remove(w.Bytes())
	return ms, nil
}
