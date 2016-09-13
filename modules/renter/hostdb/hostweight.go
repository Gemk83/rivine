package hostdb

import (
	"math/big"

	"github.com/rivine/rivine/types"
)

var (
	// Because most weights would otherwise be fractional, we set the base
	// weight to 10^150 to give ourselves lots of precision when determing the
	// weight of a host
	baseWeight = types.NewCurrency(new(big.Int).Exp(big.NewInt(10), big.NewInt(150), nil))
)

// calculateHostWeight returns the weight of a host according to the settings of
// the host database entry. Currently, only the price is considered.
func calculateHostWeight(entry hostEntry) (weight types.Currency) {
	// Prices tiered as follows:
	//    - the storage price is presented as 'per block per byte'
	//    - the contract price is presented as a flat rate
	//    - the upload bandwidth price is per byte
	//    - the download bandwidth price is per byte
	//
	// The hostdb will naively assume the following for now:
	//    - each contract covers 6 weeks of storage (default is 12 weeks, but
	//      renewals occur at midpoint) - 6048 blocks - and 10GB of storage.
	//    - uploads happen once per 12 weeks (average lifetime of a file is 12 weeks)
	//    - downloads happen once per 6 weeks (files are on average downloaded twice throughout lifetime)
	//
	// In the future, the renter should be able to track average user behavior
	// and adjust accordingly. This flexibility will be added later.
	adjustedContractPrice := entry.ContractPrice.Div64(6048).Div64(10e9) // Adjust contract price to match 10GB for 6 weeks.
	adjustedUploadPrice := entry.UploadBandwidthPrice.Div64(24192)       // Adjust upload price to match a single upload over 24 weeks.
	adjustedDownloadPrice := entry.DownloadBandwidthPrice.Div64(12096)   // Adjust download price to match one download over 12 weeks.
	siafundFee := adjustedContractPrice.Add(adjustedUploadPrice).Add(adjustedDownloadPrice).Add(entry.Collateral).MulTax()
	totalPrice := entry.StoragePrice.Add(adjustedContractPrice).Add(adjustedUploadPrice).Add(adjustedDownloadPrice).Add(siafundFee)

	// Set the weight to the base weight, and then divide it by the price
	// raised to the fifth power. This means that a host which has half the
	// total price will be 32x as likely to be selected. A host with a quarter
	// the total price will be 1024x as likely to be selected, and so on.
	weight = baseWeight
	if !totalPrice.IsZero() {
		// To avoid a divide-by-zero error, this operation is only performed on
		// non-zero prices.
		weight = baseWeight.Div(totalPrice).Div(totalPrice).Div(totalPrice).Div(totalPrice).Div(totalPrice)
	}

	// Account for collateral. Collateral has a somewhat complicated
	// relationship with price, because raising the collateral inherently
	// raises the price for renters. If the host's score increases linearly to
	// the amount of collateral they put up, then the host will gain score from
	// increasing collateral until the siafund fee makes up about 15% of the
	// total price. After that, the host will actually lose points for having
	// too much collateral.
	//
	// The renter has control over how much collateral the host uses.
	// Currently, this control is not implemented, so the hosts are penalized
	// for setting very high collateral values. Once the renter is clamping the
	// amount being spent on collateral, the hostdb can also clamp the amount
	// of collateral being taken into account by the host, to optimize the
	// host's score for the renter's needs.
	if entry.Collateral.IsZero() {
		// Instead of zeroing out the weight, just return the weight as though
		// the collateral is 1 hasting. Competitively speaking, this is
		// effectively zero.
		return weight
	}
	weight = weight.Mul(entry.Collateral)
	return weight
}
