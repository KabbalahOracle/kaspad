package utxo

import (
	"github.com/kaspanet/kaspad/domain/consensus/model"
	"github.com/kaspanet/kaspad/domain/consensus/model/externalapi"
)

type readOnlyUTXOIteratorWithDiff struct {
	baseIterator model.ReadOnlyUTXOSetIterator
	diff         *utxoDiff

	currentOutpoint  *externalapi.DomainOutpoint
	currentUTXOEntry *externalapi.UTXOEntry
	currentErr       error

	toAddIterator model.ReadOnlyUTXOSetIterator
}

// IteratorWithDiff applies a UTXODiff to given utxo iterator
func (r *readOnlyUTXOIteratorWithDiff) WithDiff(diff model.UTXODiff) (model.ReadOnlyUTXOSetIterator, error) {
	combinedDiff, err := r.diff.WithDiff(diff)
	if err != nil {
		return nil, err
	}

	return r.baseIterator.WithDiff(combinedDiff)

}

func (r *readOnlyUTXOIteratorWithDiff) Next() bool {
	for r.baseIterator.Next() { // keep looping until we reach an outpoint/entry pair that is not in r.diff.toRemove
		r.currentOutpoint, r.currentUTXOEntry, r.currentErr = r.baseIterator.Get()
		if !r.diff.toRemove.containsWithBlueScore(r.currentOutpoint, r.currentUTXOEntry.BlockBlueScore) {
			return true
		}
	}

	if r.toAddIterator.Next() {
		r.currentOutpoint, r.currentUTXOEntry, r.currentErr = r.toAddIterator.Get()
		return true
	}

	return false
}

func (r *readOnlyUTXOIteratorWithDiff) Get() (outpoint *externalapi.DomainOutpoint, utxoEntry *externalapi.UTXOEntry, err error) {
	return r.currentOutpoint, r.currentUTXOEntry, r.currentErr
}
