
# ippool - Go IP Address Pool Management Package

[![Go Reference](https://pkg.go.dev/badge/github.com/cjbrigato/ippool.svg)](https://pkg.go.dev/github.com/cjbrigato/ippool)

**ippool** is a Go package designed to manage IP address pools and leases, similar to a DHCP server but implementation-agnostic. It provides functionalities for requesting, renewing, and releasing IP addresses from defined pools, with support for sticky leases and persistence.

## Features

*   **IP Address Pool Management:**
    *   Initialize IP pools from CIDR notation (e.g., "192.168.1.0/24").
    *   Prevents overlapping IP pools during creation to ensure IP address uniqueness across pools.
    *   Allows unregistering pools to free up IP ranges for reuse in other pools.
*   **Lease Management:**
    *   Request IP addresses for devices identified by MAC addresses.
    *   Lease duration with automatic expiration and cleanup of non-sticky leases.
    *   Lease renewal functionality to extend lease durations.
    *   Explicit lease release to return IP addresses to the pool immediately.
*   **Sticky Leases:**
    *   Supports "sticky leases" where a device (identified by MAC address) preferentially gets the same IP address upon subsequent requests, even after lease expiration (as long as the IP is available).
    *   Stickiness can be requested during initial IP request or lease renewal.
    *   Sticky leases are only "lost" due to IP pool starvation, ensuring maximum IP reuse for known devices.
*   **Persistence:**
    *   Uses BoltDB, an embedded key/value database, for persistent storage of IP pool state, including pool configurations, available IPs, and leases.
    *   State is loaded on startup and saved after every state-changing operation, ensuring data durability across application restarts.
*   **Production-Ready Serialization:**
    *   Employs Protocol Buffers for efficient and robust binary serialization of data for database storage.
    *   Protocol Buffers provide schema evolution capabilities, improving long-term maintainability.
*   **Concurrency Safety:**
    *   Designed for concurrent use with internal mutex locking to protect shared state, ensuring thread-safe operations.
*   **Error Handling and Retries:**
    *   Includes robust error handling, especially around database operations.
    *   Implements retry mechanisms for database initialization, loading, and saving to handle transient database issues.

## Getting Started

### 1. Install the Package

```bash
go get github.com/cjbrigato/ippool
```

### 2. Setup BoltDB and Protobuf

*   **BoltDB:** The package uses BoltDB internally, so you don't need to install it separately as a system dependency. It's included as a Go dependency.
*   **Protocol Buffers:**
    *   **Install `protoc`:** You need the Protocol Buffer compiler (`protoc`) installed on your system. Follow the installation instructions for your operating system from the [Protocol Buffers documentation](https://developers.google.com/protocol-buffers/docs/downloads).
    *   **Install `protoc-gen-go`:** Install the Go protobuf code generator:
        ```bash
        go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
        ```
    *   **Protobuf Definitions:** The Protocol Buffer definition file (`ippool.proto`) is located in the `protos` subdirectory of the package. The generated Go files (`pb/ippool.pb.go`) are already included in the repository, so in most cases, you won't need to regenerate them.
    *   **Regenerate Protobuf Go Code (If Necessary):** If you need to regenerate the Go protobuf code (e.g., after modifying `protos/ippool.proto`), navigate to the root directory of the `ippool` package and run:
        ```bash
        protoc --go_out=pb protos/ippool.proto
        ```
        Ensure you have `protoc` and `protoc-gen-go` correctly installed and configured in your `$PATH`.

### 3. Initialize and Close the Database

In your main application, you need to initialize and close the BoltDB database:

```go
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/cjbrigato/ippool"
)

func main() {
	err := ippool.InitializeDB("") // Initialize BoltDB (default path: ippool.db)
	if err != nil {
		fmt.Println("Error initializing DB:", err)
		os.Exit(1)
	}
	defer ippool.CloseDB() // Ensure DB is closed on exit

	err = ippool.LoadPoolState() // Load existing pool state from DB on startup
	if err != nil {
		fmt.Println("Error loading pool state:", err)
		// Handle error appropriately, e.g., log and continue with a fresh state
	}

	// ... your IP pool usage code ...

}
```

## Usage Examples

### Create an IP Pool

```go
pool1, err := ippool.NewIPPool("192.168.1.0/24", 30*time.Minute)
if err != nil {
	fmt.Println("Error creating pool:", err)
	// Handle error
} else {
	fmt.Println("IP Pool created for CIDR:", pool1.ipNet)
}
```

### Request an IP Address

```go
macAddress := "AA:BB:CC:DD:EE:FF"

// Request a non-sticky IP
ip, err := pool1.RequestIP(macAddress, false)
if err != nil {
	fmt.Println("Error requesting IP:", err)
	// Handle pool exhaustion or other errors
} else {
	fmt.Printf("Leased IP (non-sticky): %s for MAC %s\n", ip, macAddress)
}

// Request a sticky IP
stickyIP, err := pool1.RequestIP(macAddress, true)
if err != nil {
	fmt.Println("Error requesting sticky IP:", err)
	// Handle error
} else {
	fmt.Printf("Leased IP (sticky): %s for MAC %s\n", stickyIP, macAddress)
}
```

### Renew a Lease

```go
renewedIP, err := pool1.RenewLease(macAddress)
if err != nil {
	fmt.Println("Error renewing lease:", err)
	// Handle error (e.g., lease not found for MAC)
} else {
	fmt.Printf("Lease renewed for MAC %s, IP: %s\n", macAddress, renewedIP)
}
```

### Release a Lease

```go
err = pool1.ReleaseLease(macAddress)
if err != nil {
	fmt.Println("Error releasing lease:", err)
	// Handle error (e.g., lease not found for MAC)
} else {
	fmt.Println("Lease released for MAC:", macAddress)
}
```

### Cleanup Expired Leases (Run Periodically)

```go
go func() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		for _, pool := range ippool.GetRegisteredPools() { // Cleanup all registered pools
			pool.CleanupExpiredLeases()
		}
	}
}()
```

### Unregister an IP Pool

```go
err = ippool.UnregisterIPPool("192.168.1.0/24")
if err != nil {
	fmt.Println("Error unregistering pool:", err)
	// Handle error (e.g., pool not found)
} else {
	fmt.Println("IP Pool unregistered for CIDR: 192.168.1.0/24")
}
```

## Persistence Details

*   **BoltDB:** The package uses BoltDB as an embedded database. The database file is by default named `ippool.db` in the current working directory. You can customize the database file path when calling `ippool.InitializeDB(path)`.
*   **Protocol Buffers:** Data is serialized using Protocol Buffers in binary format for efficient storage and retrieval in BoltDB.

## Error Handling and Logging

The package includes basic error handling and retry mechanisms for database operations. For production environments, it is recommended to:

*   Implement more comprehensive error handling based on your application's needs.
*   Use a proper logging library (e.g., `log`, `logrus`, `zap`) instead of `fmt.Println` for logging database errors and other important events.

## Concurrency

The `ippool` package is designed to be thread-safe. You can safely use it in concurrent Go routines.

## Dependencies

*   [go.etcd.io/bbolt](https://pkg.go.dev/go.etcd.io/bbolt)
*   [google.golang.org/protobuf](https://pkg.go.dev/google.golang.org/protobuf)

## License

[MIT License](LICENSE) 

---

**Contributions are welcome!**