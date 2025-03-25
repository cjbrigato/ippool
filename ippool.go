package ippool

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/cjbrigato/ippool/pb" // Assuming generated pb files are in "ippool/pb" package
	"go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"
)

// --- .proto definitions (pb/ippool.proto) ---
/*
syntax = "proto3";

package ippoolpb;

message PersistedPoolConfig {
  string cidr = 1;
  int64 lease_duration_nanos = 2;
}

message PersistedLease {
  bytes ip = 1;
  string mac = 2;
  int64 expiry_nanos = 3;
  bool sticky = 4;
  int64 last_renew_nanos = 5;
}
*/

// IPPool manages a pool of IP addresses and their leases.
type IPPool struct {
	ipNet         *net.IPNet
	ipRange       []net.IP
	availableIPs  map[string]bool  // IP address (string representation) to availability (true if available)
	leases        map[string]Lease // MAC address to Lease
	leaseDuration time.Duration
	mu            sync.RWMutex // Changed from Mutex to RWMutex
}

// Lease represents a lease for an IP address.
type Lease struct {
	IP        net.IP
	MAC       string
	Expiry    time.Time
	Sticky    bool // Indicates if this lease is sticky
	LastRenew time.Time
}

var (
	registeredPools   = make(map[string]*IPPool) // CIDR string as key
	registeredPoolsMu sync.Mutex
	db                *bbolt.DB
	dbPath            = "ippool.db" // Default database file path
	dbRetryAttempts   = 3
	dbRetryDelay      = 100 * time.Millisecond
)

const (
	poolsBucketName    = "pools"
	poolConfigBucket   = "config"
	availableIPsBucket = "available_ips"
	leasesBucket       = "leases"
)

// InitializeDB initializes the BoltDB database.
func InitializeDB(path string) error {
	if path != "" {
		dbPath = path
	}
	options := &bbolt.Options{Timeout: 1 * time.Second}
	var err error
	for i := 0; i < dbRetryAttempts; i++ {
		db, err = bbolt.Open(dbPath, 0600, options)
		if err == nil {
			return nil
		}
		// TODO: Replace fmt.Printf with a proper logger
		fmt.Printf("Error opening database (attempt %d/%d): %v, retrying in %v\n", i+1, dbRetryAttempts, err, dbRetryDelay)
		time.Sleep(dbRetryDelay)
	}
	return fmt.Errorf("failed to open database after %d attempts: %w", dbRetryAttempts, err)
}

// CloseDB closes the BoltDB database.
func CloseDB() error {
	if db != nil {
		return db.Close()
	}
	return nil
}

// LoadPoolState loads the IP pool state from the database with retry.
func LoadPoolState() error {
	for i := 0; i < dbRetryAttempts; i++ {
		err := loadPoolStateInternal()
		if err == nil {
			return nil
		}
		// TODO: Replace fmt.Printf with a proper logger
		fmt.Printf("Error loading pool state from DB (attempt %d/%d): %v, retrying in %v\n", i+1, dbRetryAttempts, err, dbRetryDelay)
		time.Sleep(dbRetryDelay)
	}
	return fmt.Errorf("failed to load pool state after %d attempts", dbRetryAttempts)
}

func loadPoolStateInternal() error {
	registeredPoolsMu.Lock()
	defer registeredPoolsMu.Unlock()

	return db.View(func(tx *bbolt.Tx) error {
		poolsBucket := tx.Bucket([]byte(poolsBucketName))
		if poolsBucket == nil {
			return nil // No pools bucket yet, fresh start
		}

		return poolsBucket.ForEach(func(cidrBytes, _ []byte) error {
			cidr := string(cidrBytes)
			poolBucket := poolsBucket.Bucket(cidrBytes)
			if poolBucket == nil {
				return fmt.Errorf("pool bucket not found for CIDR: %s", cidr)
			}

			configBytes := poolBucket.Get([]byte(poolConfigBucket))
			if configBytes == nil {
				return fmt.Errorf("pool config not found for CIDR: %s", cidr)
			}
			protoConfig := &pb.PersistedPoolConfig{}
			if err := proto.Unmarshal(configBytes, protoConfig); err != nil {
				return fmt.Errorf("failed to unmarshal pool config for CIDR %s: %w", cidr, err)
			}
			poolConfig := persistedPoolConfigFromProto(protoConfig)

			ipNet, err := parseCIDR(poolConfig.CIDR)
			if err != nil {
				return fmt.Errorf("invalid CIDR loaded from DB: %w", err)
			}
			ips, err := generateIPsFromCIDR(ipNet)
			if err != nil {
				return fmt.Errorf("error generating IPs from CIDR loaded from DB: %w", err)
			}

			// Create the pool instance. No internal lock needed here as the pool is not yet
			// accessible concurrently.
			pool := &IPPool{
				ipNet:         ipNet,
				ipRange:       ips,
				availableIPs:  make(map[string]bool),
				leases:        make(map[string]Lease),
				leaseDuration: poolConfig.LeaseDuration,
				mu:            sync.RWMutex{}, // Initialize RWMutex
			}

			// Load available IPs
			availableIPsBucket := poolBucket.Bucket([]byte(availableIPsBucket))
			if availableIPsBucket != nil {
				err := availableIPsBucket.ForEach(func(ipBytes, availableBytes []byte) error {
					available, err := decodeBool(availableBytes)
					if err != nil {
						return fmt.Errorf("failed to decode available IP status for IP %s in CIDR %s: %w", string(ipBytes), cidr, err)
					}
					pool.availableIPs[string(ipBytes)] = available
					return nil
				})
				if err != nil {
					return fmt.Errorf("error iterating available IPs bucket for CIDR %s: %w", cidr, err)
				}
			} else {
				for _, ip := range ips {
					pool.availableIPs[ip.String()] = true
				}
			}

			// Load leases
			leasesBucket := poolBucket.Bucket([]byte(leasesBucket))
			if leasesBucket != nil {
				err := leasesBucket.ForEach(func(macBytes, leaseBytes []byte) error {
					protoLease := &pb.PersistedLease{}
					if err := proto.Unmarshal(leaseBytes, protoLease); err != nil {
						return fmt.Errorf("failed to unmarshal lease for MAC %s in CIDR %s: %w", string(macBytes), cidr, err)
					}
					lease := leaseFromProto(protoLease)
					pool.leases[string(macBytes)] = lease
					return nil
				})
				if err != nil {
					return fmt.Errorf("error iterating leases bucket for CIDR %s: %w", cidr, err)
				}
			}

			registeredPools[cidr] = pool // Register the fully loaded pool
			return nil
		})
	})
}

// SavePoolState saves the IP pool state to the database with retry.
func SavePoolState() error {
	for i := 0; i < dbRetryAttempts; i++ {
		err := savePoolStateInternal()
		if err == nil {
			return nil
		}
		// TODO: Replace fmt.Printf with a proper logger
		fmt.Printf("Error saving pool state to DB (attempt %d/%d): %v, retrying in %v\n", i+1, dbRetryAttempts, err, dbRetryDelay)
		time.Sleep(dbRetryDelay)
	}
	return fmt.Errorf("failed to save pool state after %d attempts", dbRetryAttempts)
}

func savePoolStateInternal() error {
	// Lock the global registry while we iterate over it
	registeredPoolsMu.Lock()
	// Make a copy of the pointers to avoid holding the registry lock during DB operations
	poolsToSave := make([]*IPPool, 0, len(registeredPools))
	cidrsToSave := make([]string, 0, len(registeredPools))
	for cidr, pool := range registeredPools {
		poolsToSave = append(poolsToSave, pool)
		cidrsToSave = append(cidrsToSave, cidr)
	}
	registeredPoolsMu.Unlock() // Unlock the global registry

	return db.Update(func(tx *bbolt.Tx) error {
		poolsBucket, err := tx.CreateBucketIfNotExists([]byte(poolsBucketName))
		if err != nil {
			return fmt.Errorf("failed to create pools bucket: %w", err)
		}

		for i, pool := range poolsToSave {
			cidr := cidrsToSave[i]
			poolBucket, err := poolsBucket.CreateBucketIfNotExists([]byte(cidr))
			if err != nil {
				return fmt.Errorf("failed to create pool bucket for CIDR %s: %w", cidr, err)
			}

			// Acquire read lock on the individual pool while reading its state for persistence
			pool.mu.RLock()

			// Save pool config
			protoConfig := persistedPoolConfigToProto(PersistedPoolConfig{CIDR: cidr, LeaseDuration: pool.leaseDuration})
			configBytes, err := proto.Marshal(protoConfig)
			if err != nil {
				pool.mu.RUnlock()
				return fmt.Errorf("failed to marshal pool config for CIDR %s: %w", cidr, err)
			}
			if err := poolBucket.Put([]byte(poolConfigBucket), configBytes); err != nil {
				pool.mu.RUnlock()
				return fmt.Errorf("failed to put pool config for CIDR %s: %w", cidr, err)
			}

			// Save available IPs
			availableIPsBucket, err := poolBucket.CreateBucketIfNotExists([]byte(availableIPsBucket))
			if err != nil {
				pool.mu.RUnlock()
				return fmt.Errorf("failed to create available IPs bucket for CIDR %s: %w", cidr, err)
			}
			if err := availableIPsBucket.ForEach(func(k, _ []byte) error {
				return availableIPsBucket.Delete(k)
			}); err != nil {
				pool.mu.RUnlock()
				return fmt.Errorf("failed to clear available IPs bucket for CIDR %s: %w", cidr, err)
			}
			for ipStr, available := range pool.availableIPs {
				availableBytes, err := encodeBool(available)
				if err != nil {
					pool.mu.RUnlock()
					return fmt.Errorf("failed to encode available IP status for IP %s in CIDR %s: %w", ipStr, cidr, err)
				}
				if err := availableIPsBucket.Put([]byte(ipStr), availableBytes); err != nil {
					pool.mu.RUnlock()
					return fmt.Errorf("failed to put available IP status for IP %s in CIDR %s: %w", ipStr, cidr, err)
				}
			}

			// Save leases
			leasesBucket, err := poolBucket.CreateBucketIfNotExists([]byte(leasesBucket))
			if err != nil {
				pool.mu.RUnlock()
				return fmt.Errorf("failed to create leases bucket for CIDR %s: %w", cidr, err)
			}
			if err := leasesBucket.ForEach(func(k, _ []byte) error {
				return leasesBucket.Delete(k)
			}); err != nil {
				pool.mu.RUnlock()
				return fmt.Errorf("failed to clear leases bucket for CIDR %s: %w", cidr, err)
			}
			for mac, lease := range pool.leases {
				protoLease := leaseToProto(lease)
				leaseBytes, err := proto.Marshal(protoLease)
				if err != nil {
					pool.mu.RUnlock()
					return fmt.Errorf("failed to marshal lease for MAC %s in CIDR %s: %w", mac, cidr, err)
				}
				if err := leasesBucket.Put([]byte(mac), leaseBytes); err != nil {
					pool.mu.RUnlock()
					return fmt.Errorf("failed to put lease for MAC %s in CIDR %s: %w", mac, cidr, err)
				}
			}

			pool.mu.RUnlock() // Release read lock for the pool
		}
		return nil
	})
}

// PersistedPoolConfig is a struct to hold IPPool configuration for persistence.
type PersistedPoolConfig struct {
	CIDR          string
	LeaseDuration time.Duration
}

func persistedPoolConfigToProto(config PersistedPoolConfig) *pb.PersistedPoolConfig {
	return &pb.PersistedPoolConfig{
		Cidr:               config.CIDR,
		LeaseDurationNanos: config.LeaseDuration.Nanoseconds(),
	}
}

func persistedPoolConfigFromProto(protoConfig *pb.PersistedPoolConfig) PersistedPoolConfig {
	return PersistedPoolConfig{
		CIDR:          protoConfig.GetCidr(),
		LeaseDuration: time.Duration(protoConfig.GetLeaseDurationNanos()),
	}
}

func leaseToProto(lease Lease) *pb.PersistedLease {
	ipBytes := lease.IP.To4()
	if ipBytes == nil {
		ipBytes = net.IPv4zero // Should not happen in valid case, but handle for robustness
	}
	return &pb.PersistedLease{
		Ip:             ipBytes,
		Mac:            lease.MAC,
		ExpiryNanos:    lease.Expiry.UnixNano(),
		Sticky:         lease.Sticky,
		LastRenewNanos: lease.LastRenew.UnixNano(),
	}
}

func leaseFromProto(protoLease *pb.PersistedLease) Lease {
	ipBytes := protoLease.GetIp()
	var ip net.IP
	if len(ipBytes) == net.IPv4len || len(ipBytes) == net.IPv6len {
		ip = net.IP(ipBytes)
	} else {
		// Handle potential unexpected byte length, maybe default or log error
		ip = net.IPv4zero // Or some other default / error handling
	}

	return Lease{
		IP:        ip,
		MAC:       protoLease.GetMac(),
		Expiry:    time.Unix(0, protoLease.GetExpiryNanos()),
		Sticky:    protoLease.GetSticky(),
		LastRenew: time.Unix(0, protoLease.GetLastRenewNanos()),
	}
}

// --- Encoding and Decoding Functions ---

func encodeBool(value bool) ([]byte, error) {
	if value {
		return []byte{1}, nil
	}
	return []byte{0}, nil
}

func decodeBool(data []byte) (bool, error) {
	if len(data) != 1 {
		return false, fmt.Errorf("invalid bool data length: %d", len(data))
	}
	return data[0] == 1, nil
}

// NewIPPool creates a new IP pool from a CIDR string (e.g., "192.168.1.0/24")
// and a default lease duration.
func NewIPPool(cidr string, leaseDuration time.Duration) (*IPPool, error) {
	ipNet, err := parseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("invalid CIDR: %w", err)
	}

	registeredPoolsMu.Lock()
	defer registeredPoolsMu.Unlock()

	// Check for overlap with existing pools
	for registeredCIDR := range registeredPools {
		if cidrOverlaps(cidr, registeredCIDR) {
			return nil, fmt.Errorf("CIDR range %s overlaps with existing pool: %s", cidr, registeredCIDR)
		}
	}

	ips, err := generateIPsFromCIDR(ipNet)
	if err != nil {
		return nil, fmt.Errorf("error generating IPs from CIDR: %w", err)
	}

	availableIPs := make(map[string]bool)
	for _, ip := range ips {
		availableIPs[ip.String()] = true
	}

	pool := &IPPool{
		ipNet:         ipNet,
		ipRange:       ips,
		availableIPs:  availableIPs,
		leases:        make(map[string]Lease),
		leaseDuration: leaseDuration,
		mu:            sync.RWMutex{}, // Initialize RWMutex
	}
	registeredPools[cidr] = pool

	// Persist the new pool config to DB
	if err := saveNewPoolConfigToDB(pool, cidr); err != nil {
		delete(registeredPools, cidr)
		return nil, fmt.Errorf("failed to save new pool config to DB: %w", err)
	}

	return pool, nil
}

// saveNewPoolConfigToDB saves only the configuration part of a newly created pool.
// The full state (available IPs, empty leases) will be saved on the next SavePoolState call.
func saveNewPoolConfigToDB(pool *IPPool, cidr string) error {
	return db.Update(func(tx *bbolt.Tx) error {
		poolsBucket, err := tx.CreateBucketIfNotExists([]byte(poolsBucketName))
		if err != nil {
			return fmt.Errorf("failed to create pools bucket: %w", err)
		}
		poolBucket, err := poolsBucket.CreateBucketIfNotExists([]byte(cidr))
		if err != nil {
			return fmt.Errorf("failed to create pool bucket for CIDR %s: %w", cidr, err)
		}

		protoConfig := persistedPoolConfigToProto(PersistedPoolConfig{CIDR: cidr, LeaseDuration: pool.leaseDuration})
		configBytes, err := proto.Marshal(protoConfig)
		if err != nil {
			return fmt.Errorf("failed to marshal pool config for CIDR %s: %w", cidr, err)
		}
		if err := poolBucket.Put([]byte(poolConfigBucket), configBytes); err != nil {
			return fmt.Errorf("failed to put pool config for CIDR %s: %w", cidr, err)
		}
		return nil
	})
}

// UnregisterIPPool unregisters an IP pool associated with the given CIDR.
func UnregisterIPPool(cidr string) error {
	registeredPoolsMu.Lock()
	defer registeredPoolsMu.Unlock()

	if _, exists := registeredPools[cidr]; !exists {
		return fmt.Errorf("IP pool with CIDR '%s' not registered", cidr)
	}
	delete(registeredPools, cidr)

	// Remove pool from DB
	if err := deletePoolFromDB(cidr); err != nil {
		// TODO: Decide how to handle DB delete failure. Maybe re-register the pool in memory?
		return fmt.Errorf("failed to delete pool %s from DB: %w", cidr, err)
	}

	return nil
}

func deletePoolFromDB(cidr string) error {
	return db.Update(func(tx *bbolt.Tx) error {
		poolsBucket := tx.Bucket([]byte(poolsBucketName))
		if poolsBucket != nil {
			// DeleteBucket returns ErrBucketNotFound if it doesn't exist, which is okay.
			err := poolsBucket.DeleteBucket([]byte(cidr))
			if err != nil && err != bbolt.ErrBucketNotFound {
				return err
			}
		}
		return nil
	})
}

// parseCIDR, generateIPsFromCIDR, lastIP, ipToInt, intToIP, cidrOverlaps (No changes needed)
func parseCIDR(cidr string) (*net.IPNet, error) {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, err
	}
	if ipNet.IP.To4() == nil {
		return nil, fmt.Errorf("only IPv4 CIDR ranges are supported")
	}
	return ipNet, nil
}

func generateIPsFromCIDR(ipNet *net.IPNet) ([]net.IP, error) {
	var ips []net.IP
	startIP := ipNet.IP.Mask(ipNet.Mask)
	endIP := lastIP(ipNet)

	startIPInt := ipToInt(startIP)
	endIPInt := ipToInt(endIP)

	if startIPInt > endIPInt {
		return nil, fmt.Errorf("invalid CIDR range (start > end)")
	}

	// Iterate through all IPs in the range, including network and broadcast for calculation
	for ipInt := startIPInt; ipInt <= endIPInt; ipInt++ {
		ip := intToIP(ipInt)
		// Only add usable host IPs to the pool (exclude network and broadcast)
		if !ip.Equal(startIP) && !ip.Equal(endIP) {
			ips = append(ips, ip)
		}
	}
	if len(ips) == 0 && startIPInt != endIPInt && startIPInt+1 != endIPInt {
		// Handle edge cases like /31 which might have specific interpretations,
		// or small ranges where start/end are the only IPs.
		// For now, return empty if only network/broadcast exist after filtering.
		// Or, if you need to support /31, adjust logic here.
	}
	return ips, nil
}

func lastIP(ipNet *net.IPNet) net.IP {
	ip := ipNet.IP.To4()
	mask := ipNet.Mask
	lastIP := net.IP(make(net.IP, len(ip)))
	for i := 0; i < len(ip); i++ {
		lastIP[i] = ip[i] | ^mask[i]
	}
	return lastIP
}

func ipToInt(ip net.IP) uint32 {
	ip4 := ip.To4()
	if ip4 == nil {
		// Should ideally not happen for IPv4 pools, but handle defensively
		return 0
	}
	return binary.BigEndian.Uint32(ip4)
}

func intToIP(ipInt uint32) net.IP {
	ipBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(ipBytes, ipInt)
	return net.IP(ipBytes)
}

func cidrOverlaps(cidr1 string, cidr2 string) bool {
	_, ipNet1, err1 := net.ParseCIDR(cidr1)
	_, ipNet2, err2 := net.ParseCIDR(cidr2)

	if err1 != nil || err2 != nil {
		return false // Cannot determine overlap if CIDRs are invalid
	}

	// Check if one network contains the start of the other
	if ipNet1.Contains(ipNet2.IP) || ipNet2.Contains(ipNet1.IP) {
		return true
	}

	// More robust check: compare numeric range
	startIP1 := ipNet1.IP.Mask(ipNet1.Mask)
	endIP1 := lastIP(ipNet1)
	startIP2 := ipNet2.IP.Mask(ipNet2.Mask)
	endIP2 := lastIP(ipNet2)

	// Overlap occurs if range1 ends after range2 starts AND range2 ends after range1 starts
	return ipToInt(endIP1) >= ipToInt(startIP2) && ipToInt(endIP2) >= ipToInt(startIP1)
}

// RequestIP requests an IP address for a given MAC address.
func (p *IPPool) RequestIP(mac string, sticky bool) (net.IP, error) {
	p.mu.Lock() // Acquire WRITE lock for potential modifications
	defer p.mu.Unlock()

	now := time.Now()
	stateChanged := false // Track if state modification requires saving

	// 1. Check if there's an existing lease for this MAC
	if lease, ok := p.leases[mac]; ok {
		if lease.Expiry.After(now) { // Lease is still valid, renew it
			lease.Expiry = now.Add(p.leaseDuration)
			lease.LastRenew = now
			p.leases[mac] = lease
			stateChanged = true // Lease modified
			// No IP availability change
		} else { // Lease expired
			if sticky || lease.Sticky { // Sticky requested (now or before)
				// Check availability *without* releasing first for sticky
				if p.availableIPs[lease.IP.String()] { // Sticky requested and IP is available, re-assign
					p.availableIPs[lease.IP.String()] = false // Mark as taken again
					newLease := Lease{
						IP:        lease.IP,
						MAC:       mac,
						Expiry:    now.Add(p.leaseDuration),
						Sticky:    sticky || lease.Sticky,
						LastRenew: now,
					}
					p.leases[mac] = newLease
					stateChanged = true // Lease and availability modified
					// Fall through to save state if changed
				} else {
					// Sticky requested, but IP NOT available. Log and fall through to find new IP.
					// TODO: Replace fmt.Printf with a proper logger
					fmt.Printf("Sticky lease IP %s for MAC %s not available, allocating new IP if possible.\n", lease.IP, mac)
					// State hasn't changed yet, proceed to allocate new IP
				}
			} else {
				// Non-sticky expired lease: release the IP back to the pool.
				p.availableIPs[lease.IP.String()] = true
				delete(p.leases, mac)
				stateChanged = true // Lease and availability modified
				// Fall through to potentially allocate a new IP
			}
		}
		// If state changed (renewal, sticky re-assign, or non-sticky expiry cleanup), save and return if IP assigned
		if stateChanged {
			if l, ok := p.leases[mac]; ok && l.Expiry.After(now) { // Check if we successfully assigned/renewed
				if err := SavePoolState(); err != nil {
					// TODO: Replace fmt.Printf with a proper logger. Consider if error should be returned.
					fmt.Println("Error saving pool state after lease operation:", err)
				}
				return l.IP, nil
			}
			// If cleanup happened but no new IP assigned yet, continue below
		} else if lease.Expiry.After(now) { // If valid lease existed but wasn't modified (e.g., sticky IP unavailable)
			return lease.IP, nil // Return the still valid (though maybe soon expiring) IP
		}
	}

	// 2. No existing valid/renewable lease, or sticky failed, find a new available IP
	var assignedIP net.IP
	for _, ip := range p.ipRange {
		ipStr := ip.String()
		if p.availableIPs[ipStr] {
			assignedIP = ip
			p.availableIPs[ipStr] = false // Mark as taken
			stateChanged = true           // Availability modified
			break                         // Found an available IP
		}
	}

	if assignedIP == nil {
		return nil, fmt.Errorf("IP pool %s exhausted", p.ipNet.String())
	}

	// Create and store the new lease
	newLease := Lease{
		IP:        assignedIP,
		MAC:       mac,
		Expiry:    now.Add(p.leaseDuration),
		Sticky:    sticky,
		LastRenew: now,
	}
	p.leases[mac] = newLease
	stateChanged = true // Lease added

	// Save state if it was changed
	if stateChanged {
		if err := SavePoolState(); err != nil {
			// TODO: Replace fmt.Printf with a proper logger. Consider rollback or returning error?
			fmt.Println("Error saving pool state after new lease assignment:", err)
			// Even if save fails, the lease is granted in memory for now.
		}
	}
	return assignedIP, nil
}

// RenewLease renews the lease for a given MAC address.
func (p *IPPool) RenewLease(mac string) (net.IP, error) {
	p.mu.Lock() // Acquire WRITE lock
	defer p.mu.Unlock()

	lease, ok := p.leases[mac]
	if !ok {
		return nil, fmt.Errorf("no lease found for MAC address: %s", mac)
	}

	// Allow renewal even if slightly expired, similar to RequestIP logic
	// Could add a grace period check here if needed.

	lease.Expiry = time.Now().Add(p.leaseDuration)
	lease.LastRenew = time.Now()
	p.leases[mac] = lease // Update the lease

	if err := SavePoolState(); err != nil { // Persist state after renew
		// TODO: Replace fmt.Printf with a proper logger. Consider returning error?
		fmt.Println("Error saving pool state after lease renew:", err)
	}
	return lease.IP, nil
}

// ReleaseLease releases the lease for a given MAC address, making the IP available again.
func (p *IPPool) ReleaseLease(mac string) error {
	p.mu.Lock() // Acquire WRITE lock
	defer p.mu.Unlock()

	lease, ok := p.leases[mac]
	if !ok {
		// Consider if this should be an error or idempotent (return nil if not found)
		return fmt.Errorf("no lease found for MAC address: %s", mac)
	}

	p.availableIPs[lease.IP.String()] = true // Mark IP as available
	delete(p.leases, mac)                    // Remove the lease

	if err := SavePoolState(); err != nil { // Persist state after release
		// TODO: Replace fmt.Printf with a proper logger. Consider returning error?
		fmt.Println("Error saving pool state after lease release:", err)
	}
	return nil
}

// GetLease returns a *copy* of the lease information for a given MAC address, or nil if no lease exists.
func (p *IPPool) GetLease(mac string) *Lease {
	p.mu.RLock() // Acquire READ lock
	defer p.mu.RUnlock()

	lease, ok := p.leases[mac]
	if !ok {
		return nil
	}
	// Return a copy to prevent external modification of the internal lease struct
	leaseCopy := lease
	return &leaseCopy
}

// GetAllLeases returns a deep copy of all current leases in the pool.
func (p *IPPool) GetAllLeases() map[string]Lease {
	p.mu.RLock() // Acquire READ lock
	defer p.mu.RUnlock()

	leasesCopy := make(map[string]Lease, len(p.leases))
	for mac, lease := range p.leases {
		// Simple struct copy is sufficient here as Lease fields (net.IP, time.Time)
		// are either immutable-like or not modified after creation within this package.
		leasesCopy[mac] = lease
	}
	return leasesCopy
}

// CleanupExpiredLeases checks for expired leases and releases their IPs back to the pool.
func (p *IPPool) CleanupExpiredLeases() {
	p.mu.Lock() // Acquire WRITE lock
	defer p.mu.Unlock()

	now := time.Now()
	stateChanged := false // Track if state modification requires saving
	for mac, lease := range p.leases {
		if lease.Expiry.Before(now) {
			if !lease.Sticky { // Only cleanup non-sticky expired leases
				// TODO: Replace fmt.Printf with a proper logger
				fmt.Printf("Non-sticky lease for MAC %s (IP: %s) expired, releasing IP.\n", mac, lease.IP)
				p.availableIPs[lease.IP.String()] = true // Make IP available again
				delete(p.leases, mac)                    // Remove the expired lease
				stateChanged = true
			} else {
				// TODO: Replace fmt.Printf with a proper logger
				fmt.Printf("Sticky lease for MAC %s (IP: %s) expired, keeping lease (expired).\n", mac, lease.IP)
				// Do NOT release IP for sticky leases. They remain in 'leases' map.
			}
		}
	}
	if stateChanged {
		if err := SavePoolState(); err != nil { // Persist state after cleanup
			// TODO: Replace fmt.Printf with a proper logger.
			fmt.Println("Error saving pool state after cleanup:", err)
		}
	}
}

// GetRegisteredPools returns a map of currently registered pools (CIDR -> *IPPool).
// This is primarily for internal use or diagnostics.
func GetRegisteredPools() map[string]*IPPool {
	registeredPoolsMu.Lock()
	defer registeredPoolsMu.Unlock()
	poolsCopy := make(map[string]*IPPool)
	for cidr, pool := range registeredPools {
		poolsCopy[cidr] = pool
	}
	return poolsCopy
}

func GetRegisteredPool(cidr string) (*IPPool, bool) {
	registeredPoolsMu.Lock()
	defer registeredPoolsMu.Unlock()
	pool, ok := registeredPools[cidr]
	return pool, ok
}
