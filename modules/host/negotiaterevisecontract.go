package host

import (
	"net"
	"time"

	"github.com/rivine/rivine/crypto"
	"github.com/rivine/rivine/encoding"
	"github.com/rivine/rivine/modules"
	"github.com/rivine/rivine/types"
)

// managedRevisionIteration handles one iteration of the revision loop. As a
// performance optimization, multiple iterations of revisions are allowed to be
// made over the same connection.
func (h *Host) managedRevisionIteration(conn net.Conn, so *storageObligation, finalIter bool) error {
	// Send the settings to the renter. The host will keep going even if it is
	// not accepting contracts, because in this case the contract already
	// exists.
	err := h.managedRPCSettings(conn)
	if err != nil {
		return extendErr("RPCSettings failed: ", err)
	}

	// Set the negotiation deadline.
	conn.SetDeadline(time.Now().Add(modules.NegotiateFileContractRevisionTime))

	// The renter will either accept or reject the settings + revision
	// transaction. It may also return a stop response to indicate that it
	// wishes to terminate the revision loop.
	err = modules.ReadNegotiationAcceptance(conn)
	if err != nil {
		return extendErr("renter rejected settings: ", err)
	}

	// Read some variables from the host for use later in the function.
	h.mu.RLock()
	settings := h.settings
	secretKey := h.secretKey
	blockHeight := h.blockHeight
	h.mu.RUnlock()

	// The renter is going to send its intended modifications, followed by the
	// file contract revision that pays for them.
	var modifications []modules.RevisionAction
	var revision types.FileContractRevision
	err = encoding.ReadObject(conn, &modifications, settings.MaxReviseBatchSize)
	if err != nil {
		return extendErr("unable to read revision modifications: ", ErrorConnection(err.Error()))
	}
	err = encoding.ReadObject(conn, &revision, modules.NegotiateMaxFileContractRevisionSize)
	if err != nil {
		return extendErr("unable to read proposed revision: ", ErrorConnection(err.Error()))
	}

	// First read all of the modifications. Then make the modifications, but
	// with the ability to reverse them. Then verify the file contract revision
	// correctly accounts for the changes.
	var bandwidthRevenue types.Currency // Upload bandwidth.
	var storageRevenue types.Currency
	var newCollateral types.Currency
	var sectorsRemoved []crypto.Hash
	var sectorsGained []crypto.Hash
	var gainedSectorData [][]byte
	err = func() error {
		for _, modification := range modifications {
			// Check that the index points to an existing sector root. If the type
			// is ActionInsert, we permit inserting at the end.
			if modification.Type == modules.ActionInsert {
				if modification.SectorIndex > uint64(len(so.SectorRoots)) {
					return errBadModificationIndex
				}
			} else if modification.SectorIndex >= uint64(len(so.SectorRoots)) {
				return errBadModificationIndex
			}
			// Check that the data sent for the sector is not too large.
			if uint64(len(modification.Data)) > modules.SectorSize {
				return errLargeSector
			}

			switch modification.Type {
			case modules.ActionDelete:
				// There is no financial information to change, it is enough to
				// remove the sector.
				sectorsRemoved = append(sectorsRemoved, so.SectorRoots[modification.SectorIndex])
				so.SectorRoots = append(so.SectorRoots[0:modification.SectorIndex], so.SectorRoots[modification.SectorIndex+1:]...)
			case modules.ActionInsert:
				// Check that the sector size is correct.
				if uint64(len(modification.Data)) != modules.SectorSize {
					return errBadSectorSize
				}

				// Update finances.
				blocksRemaining := so.proofDeadline() - blockHeight
				blockBytesCurrency := types.NewCurrency64(uint64(blocksRemaining)).Mul64(modules.SectorSize)
				bandwidthRevenue = bandwidthRevenue.Add(settings.MinUploadBandwidthPrice.Mul64(modules.SectorSize))
				storageRevenue = storageRevenue.Add(settings.MinStoragePrice.Mul(blockBytesCurrency))
				newCollateral = newCollateral.Add(settings.Collateral.Mul(blockBytesCurrency))

				// Insert the sector into the root list.
				newRoot := crypto.MerkleRoot(modification.Data)
				sectorsGained = append(sectorsGained, newRoot)
				gainedSectorData = append(gainedSectorData, modification.Data)
				so.SectorRoots = append(so.SectorRoots[:modification.SectorIndex], append([]crypto.Hash{newRoot}, so.SectorRoots[modification.SectorIndex:]...)...)
			case modules.ActionModify:
				// Check that the offset and length are okay. Length is already
				// known to be appropriately small, but the offset needs to be
				// checked for being appropriately small as well otherwise there is
				// a risk of overflow.
				if modification.Offset > modules.SectorSize || modification.Offset+uint64(len(modification.Data)) > modules.SectorSize {
					return errIllegalOffsetAndLength
				}

				// Get the data for the new sector.
				sector, err := h.ReadSector(so.SectorRoots[modification.SectorIndex])
				if err != nil {
					return extendErr("could not read sector: ", ErrorInternal(err.Error()))
				}
				copy(sector[modification.Offset:], modification.Data)

				// Update finances.
				bandwidthRevenue = bandwidthRevenue.Add(settings.MinUploadBandwidthPrice.Mul64(uint64(len(modification.Data))))

				// Update the sectors removed and gained to indicate that the old
				// sector has been replaced with a new sector.
				newRoot := crypto.MerkleRoot(sector)
				sectorsRemoved = append(sectorsRemoved, so.SectorRoots[modification.SectorIndex])
				sectorsGained = append(sectorsGained, newRoot)
				gainedSectorData = append(gainedSectorData, sector)
				so.SectorRoots[modification.SectorIndex] = newRoot
			default:
				return errUnknownModification
			}
		}
		newRevenue := storageRevenue.Add(bandwidthRevenue)
		return extendErr("unable to verify revision: ", verifyRevision(*so, revision, blockHeight, newRevenue, newCollateral))
	}()
	if err != nil {
		modules.WriteNegotiationRejection(conn, err) // Error is ignored so that the error type can be preserved in extendErr.
		return extendErr("rejected proposed modifications: ", err)
	}
	// Revision is acceptable, write an acceptance string.
	err = modules.WriteNegotiationAcceptance(conn)
	if err != nil {
		return extendErr("could not accept revision modifications: ", ErrorConnection(err.Error()))
	}

	// Renter will send a transaction signature for the file contract revision.
	var renterSig types.TransactionSignature
	err = encoding.ReadObject(conn, &renterSig, modules.NegotiateMaxTransactionSignatureSize)
	if err != nil {
		return extendErr("could not read renter transaction signature: ", ErrorConnection(err.Error()))
	}
	// Verify that the signature is valid and get the host's signature.
	txn, err := createRevisionSignature(revision, renterSig, secretKey, blockHeight)
	if err != nil {
		modules.WriteNegotiationRejection(conn, err) // Error is ignored so that the error type can be preserved in extendErr.
		return extendErr("could not create revision signature: ", err)
	}

	so.PotentialStorageRevenue = so.PotentialStorageRevenue.Add(storageRevenue)
	so.RiskedCollateral = so.RiskedCollateral.Add(newCollateral)
	so.PotentialUploadRevenue = so.PotentialUploadRevenue.Add(bandwidthRevenue)
	so.RevisionTransactionSet = []types.Transaction{txn}
	err = h.modifyStorageObligation(*so, sectorsRemoved, sectorsGained, gainedSectorData)
	if err != nil {
		modules.WriteNegotiationRejection(conn, err) // Error is ignored so that the error type can be preserved in extendErr.
		return extendErr("could not modify storage obligation: ", ErrorInternal(err.Error()))
	}

	// Host will now send acceptance and its signature to the renter. This
	// iteration is complete. If the finalIter flag is set, StopResponse will
	// be sent instead. This indicates to the renter that the host wishes to
	// terminate the revision loop.
	if finalIter {
		err = modules.WriteNegotiationStop(conn)
	} else {
		err = modules.WriteNegotiationAcceptance(conn)
	}
	if err != nil {
		return extendErr("iteration signal failed to send: ", ErrorConnection(err.Error()))
	}
	err = encoding.WriteObject(conn, txn.TransactionSignatures[1])
	if err != nil {
		return extendErr("failed to write revision signatures: ", ErrorConnection(err.Error()))
	}
	return nil
}

// managedRPCReviseContract accepts a request to revise an existing contract.
// Revisions can add sectors, delete sectors, and modify existing sectors.
func (h *Host) managedRPCReviseContract(conn net.Conn) error {
	// Set a preliminary deadline for receiving the storage obligation.
	startTime := time.Now()
	// Perform the file contract revision exchange, giving the renter the most
	// recent file contract revision and getting the storage obligation that
	// will be used to pay for the data.
	_, so, err := h.managedRPCRecentRevision(conn)
	if err != nil {
		return extendErr("RPCRecentRevision failed: ", err)
	}
	// The storage obligation is received with a lock on it. Defer a call to
	// unlock the storage obligation.
	defer func() {
		h.managedUnlockStorageObligation(so.id())
	}()

	// Begin the revision loop. The host will process revisions until a
	// timeout is reached, or until the renter sends a StopResponse.
	for timeoutReached := false; !timeoutReached; {
		timeoutReached = time.Since(startTime) > iteratedConnectionTime
		err := h.managedRevisionIteration(conn, &so, timeoutReached)
		if err == modules.ErrStopResponse {
			return nil
		} else if err != nil {
			return extendErr("revision iteration failed: ", err)
		}
	}
	return nil
}

// verifyRevision checks that the revision pays the host correctly, and that
// the revision does not attempt any malicious or unexpected changes.
func verifyRevision(so storageObligation, revision types.FileContractRevision, blockHeight types.BlockHeight, newRevenue, newCollateral types.Currency) error {
	// Check that the revision is well-formed.
	if len(revision.NewValidProofOutputs) != 2 || len(revision.NewMissedProofOutputs) != 3 {
		return errBadContractOutputCounts
	}

	// Check that the time to finalize and submit the file contract revision
	// has not already passed.
	if so.expiration()-revisionSubmissionBuffer <= blockHeight {
		return errLateRevision
	}

	oldFCR := so.RevisionTransactionSet[len(so.RevisionTransactionSet)-1].FileContractRevisions[0]

	// Check that all non-volatile fields are the same.
	if oldFCR.ParentID != revision.ParentID {
		return errBadContractParent
	}
	if oldFCR.UnlockConditions.UnlockHash() != revision.UnlockConditions.UnlockHash() {
		return errBadUnlockConditions
	}
	if oldFCR.NewRevisionNumber >= revision.NewRevisionNumber {
		return errBadRevisionNumber
	}
	if revision.NewFileSize != uint64(len(so.SectorRoots))*modules.SectorSize {
		return errBadFileSize
	}
	if oldFCR.NewWindowStart != revision.NewWindowStart {
		return errBadWindowStart
	}
	if oldFCR.NewWindowEnd != revision.NewWindowEnd {
		return errBadWindowEnd
	}
	if oldFCR.NewUnlockHash != revision.NewUnlockHash {
		return errBadUnlockHash
	}

	// The new revenue comes out of the renter's valid outputs.
	if revision.NewValidProofOutputs[0].Value.Add(newRevenue).Cmp(oldFCR.NewValidProofOutputs[0].Value) > 0 {
		return errHighRenterValidOutput
	}
	// The new revenue goes into the host's valid outputs.
	if oldFCR.NewValidProofOutputs[1].Value.Add(newRevenue).Cmp(revision.NewValidProofOutputs[1].Value) < 0 {
		return errLowHostValidOutput
	}
	// The new revenue comes out of the renter's missed outputs.
	if revision.NewMissedProofOutputs[0].Value.Add(newRevenue).Cmp(oldFCR.NewMissedProofOutputs[0].Value) > 0 {
		return errHighRenterMissedOutput
	}
	// The new collateral comes out of the host's missed outputs.
	if revision.NewMissedProofOutputs[1].Value.Add(newCollateral).Cmp(oldFCR.NewMissedProofOutputs[1].Value) < 0 {
		return errLowHostMissedOutput
	}

	// The Merkle root is checked last because it is the most expensive check.
	log2SectorSize := uint64(0)
	for 1<<log2SectorSize < (modules.SectorSize / crypto.SegmentSize) {
		log2SectorSize++
	}
	ct := crypto.NewCachedTree(log2SectorSize)
	for _, root := range so.SectorRoots {
		ct.Push(root)
	}
	expectedMerkleRoot := ct.Root()
	if revision.NewFileMerkleRoot != expectedMerkleRoot {
		return errBadFileMerkleRoot
	}

	return nil
}
