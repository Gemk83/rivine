package consensus

// database_test.go contains a bunch of legacy functions to preserve
// compatibility with the test suite.

import (
	"github.com/rivine/rivine/encoding"
	"github.com/rivine/rivine/types"

	"github.com/NebulousLabs/bolt"
)

// dbBlockHeight is a convenience function allowing blockHeight to be called
// without a bolt.Tx.
func (cs *ConsensusSet) dbBlockHeight() (bh types.BlockHeight) {
	dbErr := cs.db.View(func(tx *bolt.Tx) error {
		bh = blockHeight(tx)
		return nil
	})
	if dbErr != nil {
		panic(dbErr)
	}
	return bh
}

// dbCurrentBlockID is a convenience function allowing currentBlockID to be
// called without a bolt.Tx.
func (cs *ConsensusSet) dbCurrentBlockID() (id types.BlockID) {
	dbErr := cs.db.View(func(tx *bolt.Tx) error {
		id = currentBlockID(tx)
		return nil
	})
	if dbErr != nil {
		panic(dbErr)
	}
	return id
}

// dbCurrentProcessedBlock is a convenience function allowing
// currentProcessedBlock to be called without a bolt.Tx.
func (cs *ConsensusSet) dbCurrentProcessedBlock() (pb *processedBlock) {
	dbErr := cs.db.View(func(tx *bolt.Tx) error {
		pb = currentProcessedBlock(tx)
		return nil
	})
	if dbErr != nil {
		panic(dbErr)
	}
	return pb
}

// dbGetPath is a convenience function allowing getPath to be called without a
// bolt.Tx.
func (cs *ConsensusSet) dbGetPath(bh types.BlockHeight) (id types.BlockID, err error) {
	dbErr := cs.db.View(func(tx *bolt.Tx) error {
		id, err = getPath(tx, bh)
		return nil
	})
	if dbErr != nil {
		panic(dbErr)
	}
	return id, err
}

// dbPushPath is a convenience function allowing pushPath to be called without a
// bolt.Tx.
func (cs *ConsensusSet) dbPushPath(bid types.BlockID) {
	dbErr := cs.db.Update(func(tx *bolt.Tx) error {
		pushPath(tx, bid)
		return nil
	})
	if dbErr != nil {
		panic(dbErr)
	}
}

// dbGetBlockMap is a convenience function allowing getBlockMap to be called
// without a bolt.Tx.
func (cs *ConsensusSet) dbGetBlockMap(id types.BlockID) (pb *processedBlock, err error) {
	dbErr := cs.db.View(func(tx *bolt.Tx) error {
		pb, err = getBlockMap(tx, id)
		return nil
	})
	if dbErr != nil {
		panic(dbErr)
	}
	return pb, err
}

// dbGetSiacoinOutput is a convenience function allowing getSiacoinOutput to be
// called without a bolt.Tx.
func (cs *ConsensusSet) dbGetSiacoinOutput(id types.SiacoinOutputID) (sco types.SiacoinOutput, err error) {
	dbErr := cs.db.View(func(tx *bolt.Tx) error {
		sco, err = getSiacoinOutput(tx, id)
		return nil
	})
	if dbErr != nil {
		panic(dbErr)
	}
	return sco, err
}

// getArbSiacoinOutput is a convenience function fetching a single random
// siacoin output from the database.
func (cs *ConsensusSet) getArbSiacoinOutput() (scoid types.SiacoinOutputID, sco types.SiacoinOutput, err error) {
	dbErr := cs.db.View(func(tx *bolt.Tx) error {
		cursor := tx.Bucket(SiacoinOutputs).Cursor()
		scoidBytes, scoBytes := cursor.First()
		copy(scoid[:], scoidBytes)
		return encoding.Unmarshal(scoBytes, &sco)
	})
	if dbErr != nil {
		panic(dbErr)
	}
	if err != nil {
		return types.SiacoinOutputID{}, types.SiacoinOutput{}, err
	}
	return scoid, sco, nil
}

// dbGetBlockStakeOutput is a convenience function allowing getSiafundOutput to be
// called without a bolt.Tx.
func (cs *ConsensusSet) dbGetBlockStakeOutput(id types.BlockStakeOutputID) (sfo types.BlockStakeOutput, err error) {
	dbErr := cs.db.View(func(tx *bolt.Tx) error {
		sfo, err = getBlockStakeOutput(tx, id)
		return nil
	})
	if dbErr != nil {
		panic(dbErr)
	}
	return sfo, err
}

// dbAddBlockStakeOutput is a convenience function allowing addBlockStakeOutput to be
// called without a bolt.Tx.
func (cs *ConsensusSet) dbAddBlockStakeOutput(id types.BlockStakeOutputID, sfo types.BlockStakeOutput) {
	dbErr := cs.db.Update(func(tx *bolt.Tx) error {
		addBlockStakeOutput(tx, id, sfo)
		return nil
	})
	if dbErr != nil {
		panic(dbErr)
	}
}

// dbGetDSCO is a convenience function allowing a delayed siacoin output to be
// fetched without a bolt.Tx. An error is returned if the delayed output is not
// found at the maturity height indicated by the input.
func (cs *ConsensusSet) dbGetDSCO(height types.BlockHeight, id types.SiacoinOutputID) (dsco types.SiacoinOutput, err error) {
	dbErr := cs.db.View(func(tx *bolt.Tx) error {
		dscoBucketID := append(prefixDSCO, encoding.Marshal(height)...)
		dscoBucket := tx.Bucket(dscoBucketID)
		if dscoBucket == nil {
			err = errNilItem
			return nil
		}
		dscoBytes := dscoBucket.Get(id[:])
		if dscoBytes == nil {
			err = errNilItem
			return nil
		}
		err = encoding.Unmarshal(dscoBytes, &dsco)
		if err != nil {
			panic(err)
		}
		return nil
	})
	if dbErr != nil {
		panic(dbErr)
	}
	return dsco, err
}
