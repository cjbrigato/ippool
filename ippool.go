package ippool

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/cjbrigato/ippool/pb"

	"go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"
)

// IPPool manages a pool of IP addresses and their leases.
type IPPool struct {
	ipNet         *net.IPNet
	ipRange       []net.IP
	availableIPs  map[string]bool  // IP address (string representation) to availability (true if available)
	leases        map[string]Lease // MAC address to Lease
	leaseDuration time.Duration
	mu            sync.Mutex
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

			pool := &IPPool{
				ipNet:         ipNet,
				ipRange:       ips,
				availableIPs:  make(map[string]bool),
				leases:        make(map[string]Lease),
				leaseDuration: poolConfig.LeaseDuration,
				mu:            sync.Mutex{},
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

			registeredPools[cidr] = pool
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
		fmt.Printf("Error saving pool state to DB (attempt %d/%d): %v, retrying in %v\n", i+1, dbRetryAttempts, err, dbRetryDelay)
		time.Sleep(dbRetryDelay)
	}
	return fmt.Errorf("failed to save pool state after %d attempts", dbRetryAttempts)
}

func savePoolStateInternal() error {
	registeredPoolsMu.Lock()
	defer registeredPoolsMu.Unlock()

	return db.Update(func(tx *bbolt.Tx) error {
		poolsBucket, err := tx.CreateBucketIfNotExists([]byte(poolsBucketName))
		if err != nil {
			return fmt.Errorf("failed to create pools bucket: %w", err)
		}

		for cidr, pool := range registeredPools {
			poolBucket, err := poolsBucket.CreateBucketIfNotExists([]byte(cidr))
			if err != nil {
				return fmt.Errorf("failed to create pool bucket for CIDR %s: %w", cidr, err)
			}

			// Save pool config
			protoConfig := persistedPoolConfigToProto(PersistedPoolConfig{CIDR: cidr, LeaseDuration: pool.leaseDuration})
			configBytes, err := proto.Marshal(protoConfig)
			if err != nil {
				return fmt.Errorf("failed to marshal pool config for CIDR %s: %w", cidr, err)
			}
			if err := poolBucket.Put([]byte(poolConfigBucket), configBytes); err != nil {
				return fmt.Errorf("failed to put pool config for CIDR %s: %w", cidr, err)
			}

			// Save available IPs
			availableIPsBucket, err := poolBucket.CreateBucketIfNotExists([]byte(availableIPsBucket))
			if err != nil {
				return fmt.Errorf("failed to create available IPs bucket for CIDR %s: %w", cidr, err)
			}
			if err := availableIPsBucket.ForEach(func(k, _ []byte) error {
				return availableIPsBucket.Delete(k)
			}); err != nil {
				return fmt.Errorf("failed to clear available IPs bucket for CIDR %s: %w", cidr, err)
			}
			for ipStr, available := range pool.availableIPs {
				availableBytes, err := encodeBool(available)
				if err != nil {
					return fmt.Errorf("failed to encode available IP status for IP %s in CIDR %s: %w", ipStr, cidr, err)
				}
				if err := availableIPsBucket.Put([]byte(ipStr), availableBytes); err != nil {
					return fmt.Errorf("failed to put available IP status for IP %s in CIDR %s: %w", ipStr, cidr, err)
				}
			}

			// Save leases
			leasesBucket, err := poolBucket.CreateBucketIfNotExists([]byte(leasesBucket))
			if err != nil {
				return fmt.Errorf("failed to create leases bucket for CIDR %s: %w", cidr, err)
			}
			if err := leasesBucket.ForEach(func(k, _ []byte) error {
				return leasesBucket.Delete(k)
			}); err != nil {
				return fmt.Errorf("failed to clear leases bucket for CIDR %s: %w", cidr, err)
			}
			for mac, lease := range pool.leases {
				protoLease := leaseToProto(lease)
				leaseBytes, err := proto.Marshal(protoLease)
				if err != nil {
					return fmt.Errorf("failed to marshal lease for MAC %s in CIDR %s: %w", mac, cidr, err)
				}
				if err := leasesBucket.Put([]byte(mac), leaseBytes); err != nil {
					return fmt.Errorf("failed to put lease for MAC %s in CIDR %s: %w", mac, cidr, err)
				}
			}
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
	return Lease{
		IP:        net.IP(protoLease.GetIp()), // Assuming GetIp() returns 4 bytes
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
			return nil, fmt.Errorf("CIDR range overlaps with existing pool: %s", registeredCIDR)
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
		mu:            sync.Mutex{},
	}
	registeredPools[cidr] = pool

	// Persist the new pool to DB
	if err := saveNewPoolToDB(pool, cidr); err != nil {
		delete(registeredPools, cidr)
		return nil, fmt.Errorf("failed to save new pool to DB: %w", err)
	}

	return pool, nil
}

func saveNewPoolToDB(pool *IPPool, cidr string) error {
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
// After unregistering, the IPs in this CIDR can be used in new IP pools.
func UnregisterIPPool(cidr string) error {
	registeredPoolsMu.Lock()
	defer registeredPoolsMu.Unlock()

	if _, exists := registeredPools[cidr]; !exists {
		return fmt.Errorf("IP pool with CIDR '%s' not registered", cidr)
	}
	delete(registeredPools, cidr)

	// Remove pool from DB
	if err := deletePoolFromDB(cidr); err != nil {
		return fmt.Errorf("failed to delete pool from DB: %w", err)
	}

	return nil
}

func deletePoolFromDB(cidr string) error {
	return db.Update(func(tx *bbolt.Tx) error {
		poolsBucket := tx.Bucket([]byte(poolsBucketName))
		if poolsBucket != nil {
			return poolsBucket.DeleteBucket([]byte(cidr))
		}
		return nil
	})
}

// parseCIDR, generateIPsFromCIDR, lastIP, ipToInt, intToIP, cidrOverlaps  (No changes needed from previous version)
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

	if ipToInt(startIP) > ipToInt(endIP) {
		return nil, fmt.Errorf("invalid CIDR range")
	}

	for ipInt := ipToInt(startIP); ipInt <= ipToInt(endIP); ipInt++ {
		ip := intToIP(ipInt)
		if !ip.Equal(startIP) && !ip.Equal(endIP) {
			ips = append(ips, ip)
		}
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
		return false
	}

	if ipNet1.Contains(ipNet2.IP) || ipNet2.Contains(ipNet1.IP) {
		return true
	}
	startIP1 := ipNet1.IP.Mask(ipNet1.Mask)
	endIP1 := lastIP(ipNet1)
	startIP2 := ipNet2.IP.Mask(ipNet2.Mask)
	endIP2 := lastIP(ipNet2)

	return ipToInt(endIP1) >= ipToInt(startIP2) && ipToInt(endIP2) >= ipToInt(startIP1)
}

// RequestIP requests an IP address for a given MAC address.
func (p *IPPool) RequestIP(mac string, sticky bool) (net.IP, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()

	// 1. Check if there's an existing lease for this MAC
	if lease, ok := p.leases[mac]; ok {
		if lease.Expiry.After(now) {
			lease.Expiry = now.Add(p.leaseDuration)
			lease.LastRenew = now
			p.leases[mac] = lease
			if err := SavePoolState(); err != nil {
				fmt.Println("Error saving pool state after lease renew:", err)
			}
			return lease.IP, nil
		} else {
			if sticky || lease.Sticky {
				if p.availableIPs[lease.IP.String()] {
					p.availableIPs[lease.IP.String()] = false
					newLease := Lease{
						IP:        lease.IP,
						MAC:       mac,
						Expiry:    now.Add(p.leaseDuration),
						Sticky:    sticky || lease.Sticky,
						LastRenew: now,
					}
					p.leases[mac] = newLease
					if err := SavePoolState(); err != nil {
						fmt.Println("Error saving pool state after sticky re-assign:", err)
					}
					return lease.IP, nil
				} else {
					fmt.Printf("Sticky lease IP %s for MAC %s not available, allocating new IP if possible.\n", lease.IP, mac)
				}
			} else {
				p.availableIPs[lease.IP.String()] = true
				delete(p.leases, mac)
			}
		}
	}

	// 2. No existing valid lease, find a new available IP
	var assignedIP net.IP
	for _, ip := range p.ipRange {
		ipStr := ip.String()
		if p.availableIPs[ipStr] {
			assignedIP = ip
			p.availableIPs[ipStr] = false
			break
		}
	}

	if assignedIP == nil {
		return nil, fmt.Errorf("IP pool exhausted")
	}

	newLease := Lease{
		IP:        assignedIP,
		MAC:       mac,
		Expiry:    now.Add(p.leaseDuration),
		Sticky:    sticky,
		LastRenew: now,
	}
	p.leases[mac] = newLease
	if err := SavePoolState(); err != nil {
		fmt.Println("Error saving pool state after new lease:", err)
	}
	return assignedIP, nil
}

// RenewLease renews the lease for a given MAC address.
func (p *IPPool) RenewLease(mac string) (net.IP, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	lease, ok := p.leases[mac]
	if !ok {
		return nil, fmt.Errorf("no lease found for MAC address: %s", mac)
	}

	lease.Expiry = time.Now().Add(p.leaseDuration)
	lease.LastRenew = time.Now()
	p.leases[mac] = lease
	if err := SavePoolState(); err != nil {
		fmt.Println("Error saving pool state after lease renew:", err)
	}
	return lease.IP, nil
}

// ReleaseLease releases the lease for a given MAC address, making the IP available again.
func (p *IPPool) ReleaseLease(mac string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	lease, ok := p.leases[mac]
	if !ok {
		return fmt.Errorf("no lease found for MAC address: %s", mac)
	}

	p.availableIPs[lease.IP.String()] = true
	delete(p.leases, mac)
	if err := SavePoolState(); err != nil {
		fmt.Println("Error saving pool state after lease release:", err)
	}
	return nil
}

// GetLease returns the lease information for a given MAC address, or nil if no lease exists.
func (p *IPPool) GetLease(mac string) *Lease {
	p.mu.Lock()
	defer p.mu.Unlock()
	lease, ok := p.leases[mac]
	if !ok {
		return nil
	}
	return &lease
}

// CleanupExpiredLeases checks for expired leases and releases their IPs back to the pool.
func (p *IPPool) CleanupExpiredLeases() {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	leasesChanged := false
	for mac, lease := range p.leases {
		if lease.Expiry.Before(now) {
			if !lease.Sticky {
				fmt.Printf("Non-sticky lease for MAC %s (IP: %s) expired, releasing IP.\n", mac, lease.IP)
				p.availableIPs[lease.IP.String()] = true
				delete(p.leases, mac)
				leasesChanged = true
			} else {
				fmt.Printf("Sticky lease for MAC %s (IP: %s) expired, keeping lease (expired).\n", mac, lease.IP)
			}
		}
	}
	if leasesChanged {
		if err := SavePoolState(); err != nil {
			fmt.Println("Error saving pool state after cleanup:", err)
		}
	}
}

// GetRegisteredPools for external access (e.g., cleanup routines)
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
