package hostdb

// scan.go contains the functions which periodically scan the list of all hosts
// to see which hosts are online or offline, and to get any updates to the
// settings of the hosts.

import (
	"bytes"
	"crypto/rand"
	"math/big"
	"time"

	"github.com/rivine/rivine/build"
	"github.com/rivine/rivine/crypto"
	"github.com/rivine/rivine/encoding"
	"github.com/rivine/rivine/modules"
	"github.com/rivine/rivine/types"
)

const (
	defaultScanSleep = 1*time.Hour + 37*time.Minute
	maxScanSleep     = 4 * time.Hour
	minScanSleep     = 1 * time.Hour

	maxActiveHosts              = 500
	inactiveHostCheckupQuantity = 250

	maxSettingsLen = 2e3

	hostRequestTimeout = 5 * time.Second

	// scanningThreads is the number of threads that will be probing hosts for
	// their settings and checking for reliability.
	scanningThreads = 25
)

// Reliability is a measure of a host's uptime.
var (
	MaxReliability     = types.NewCurrency64(500) // Given the scanning defaults, about 6 weeks of survival.
	DefaultReliability = types.NewCurrency64(150) // Given the scanning defaults, about 2 week of survival.
	UnreachablePenalty = types.NewCurrency64(1)
)

// addHostToScanPool creates a gofunc that adds a host to the scan pool. If the
// scan pool is currently full, the blocking gofunc will not cause a deadlock.
// The gofunc is created inside of this function to eliminate the burden of
// needing to remember to call 'go addHostToScanPool'.
func (hdb *HostDB) scanHostEntry(entry *hostEntry) {
	go func() {
		hdb.scanPool <- entry
	}()
}

// decrementReliability reduces the reliability of a node, moving it out of the
// set of active hosts or deleting it entirely if necessary.
func (hdb *HostDB) decrementReliability(addr modules.NetAddress, penalty types.Currency) {
	// Look up the entry and decrement the reliability.
	entry, exists := hdb.allHosts[addr]
	if !exists {
		// TODO: should panic here
		return
	}
	entry.Reliability = entry.Reliability.Sub(penalty)
	entry.Online = false

	// If the entry is in the active database, remove it from the active
	// database.
	node, exists := hdb.activeHosts[addr]
	if exists {
		node.removeNode()
		delete(hdb.activeHosts, entry.NetAddress)
	}

	// If the reliability has fallen to 0, remove the host from the
	// database entirely.
	if entry.Reliability.IsZero() {
		delete(hdb.allHosts, addr)
	}
}

// managedUpdateEntry updates an entry in the hostdb after a scan has taken
// place.
func (hdb *HostDB) managedUpdateEntry(entry *hostEntry, newSettings modules.HostExternalSettings, netErr error) {
	hdb.mu.Lock()
	defer hdb.mu.Unlock()

	// Regardless of whether the host responded, add it to allHosts.
	priorHost, exists := hdb.allHosts[entry.NetAddress]
	if !exists {
		hdb.allHosts[entry.NetAddress] = entry
	}

	// If the scan was unsuccessful, decrement the host's reliability.
	if netErr != nil {
		if exists && bytes.Equal(priorHost.PublicKey.Key, entry.PublicKey.Key) {
			// Only decrement the reliability if the public key in the
			// hostdb matches the public key in the host announcement -
			// the failure may just be a failed signature, indicating
			// the wrong public key.
			hdb.decrementReliability(entry.NetAddress, UnreachablePenalty)
		}
		return
	}

	// The host entry should be updated to reflect the new weight. The safety
	// properties of the tree require that the weight does not change while the
	// node is in the tree, so the node must be removed before the settings and
	// weight are changed.
	existingNode, exists := hdb.activeHosts[entry.NetAddress]
	if exists {
		existingNode.removeNode()
		delete(hdb.activeHosts, entry.NetAddress)
	}

	// Update the host settings, reliability, and weight. The old NetAddress
	// must be preserved.
	newSettings.NetAddress = entry.HostExternalSettings.NetAddress
	entry.HostExternalSettings = newSettings
	entry.Reliability = MaxReliability
	entry.Weight = calculateHostWeight(*entry)
	entry.Online = true

	// If 'maxActiveHosts' has not been reached, add the host to the
	// activeHosts tree.
	if _, exists := hdb.activeHosts[entry.NetAddress]; exists || len(hdb.activeHosts) < maxActiveHosts {
		hdb.insertNode(entry)
	}
	hdb.save()
}

// threadedProbeHosts tries to fetch the settings of a host. If successful, the
// host is put in the set of active hosts. If unsuccessful, the host id deleted
// from the set of active hosts.
func (hdb *HostDB) threadedProbeHosts() {
	defer hdb.threadGroup.Done()
	for hostEntry := range hdb.scanPool {
		// Request settings from the queued host entry.
		// TODO: use dialer.Cancel to shutdown quickly
		//
		// A readlock is necessary when viewing the elements of the host entry.
		hdb.mu.RLock()
		netAddr := hostEntry.NetAddress
		pubKey := hostEntry.PublicKey
		hdb.mu.RUnlock()
		hdb.log.Debugln("Scanning", netAddr, pubKey)
		var settings modules.HostExternalSettings
		err := func() error {
			conn, err := hdb.dialer.DialTimeout(netAddr, hostRequestTimeout)
			if err != nil {
				return err
			}
			defer conn.Close()
			err = encoding.WriteObject(conn, modules.RPCSettings)
			if err != nil {
				return err
			}
			var pubkey crypto.PublicKey
			copy(pubkey[:], pubKey.Key)
			return crypto.ReadSignedObject(conn, &settings, maxSettingsLen, pubkey)
		}()
		if err != nil {
			hdb.log.Debugln("Scanning", netAddr, pubKey, "failed", err)
		} else {
			hdb.log.Debugln("Scanning", netAddr, pubKey, "succeeded")
		}

		// Update the host tree to have a new entry.
		hdb.managedUpdateEntry(hostEntry, settings, err)
	}
}

// threadedScan is an ongoing function which will query the full set of hosts
// every few hours to see who is online and available for uploading.
func (hdb *HostDB) threadedScan() {
	defer hdb.threadGroup.Done()
	for {
		// Determine who to scan. At most 'maxActiveHosts' will be scanned,
		// starting with the active hosts followed by a random selection of the
		// inactive hosts.
		func() {
			hdb.mu.Lock()
			defer hdb.mu.Unlock()

			// Scan all active hosts.
			for _, host := range hdb.activeHosts {
				hdb.scanHostEntry(host.hostEntry)
			}

			// Assemble all of the inactive hosts into a single array.
			var entries []*hostEntry
			for _, entry := range hdb.allHosts {
				_, exists := hdb.activeHosts[entry.NetAddress]
				if !exists {
					entries = append(entries, entry)
				}
			}

			// Generate a random ordering of up to inactiveHostCheckupQuantity
			// hosts.
			hostOrder, err := crypto.Perm(len(entries))
			if err != nil {
				hdb.log.Println("ERR: could not generate random permutation:", err)
			}

			// Scan each host.
			for i := 0; i < len(hostOrder) && i < inactiveHostCheckupQuantity; i++ {
				hdb.scanHostEntry(entries[hostOrder[i]])
			}
		}()

		// Sleep for a random amount of time before doing another round of
		// scanning. The minimums and maximums keep the scan time reasonable,
		// while the randomness prevents the scanning from always happening at
		// the same time of day or week.
		maxBig := big.NewInt(int64(maxScanSleep))
		minBig := big.NewInt(int64(minScanSleep))
		randSleep, err := rand.Int(rand.Reader, maxBig.Sub(maxBig, minBig))
		if err != nil {
			build.Critical(err)
			// If there's an error, sleep for the default amount of time.
			defaultBig := big.NewInt(int64(defaultScanSleep))
			randSleep = defaultBig.Sub(defaultBig, minBig)
		}

		select {
		// awaken and exit if hostdb is closing
		case <-hdb.closeChan:
			return
		case <-time.After(time.Duration(randSleep.Int64()) + minScanSleep):
		}
	}
}
