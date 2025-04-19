package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/grandcat/zeroconf"
)

type ServiceInfo struct {
	Name      string
	Type      string
	Domain    string
	Host      string
	IPv4      []net.IP
	IPv6      []net.IP
	Port      int
	TTL       uint32
	Timestamp time.Time
	Metadata  map[string]string
}

type ServiceRegistry struct {
	services map[string]ServiceInfo
	mutex    sync.RWMutex
}

func NewServiceRegistry() *ServiceRegistry {
	return &ServiceRegistry{
		services: make(map[string]ServiceInfo),
	}
}

func (r *ServiceRegistry) AddOrUpdate(service ServiceInfo) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	key := fmt.Sprintf("%s.%s.%s", service.Name, service.Type, service.Domain)
	r.services[key] = service
	log.Printf("Added/updated service in registry: %s", key)
}

func (r *ServiceRegistry) Remove(name, serviceType, domain string) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	key := fmt.Sprintf("%s.%s.%s", name, serviceType, domain)
	delete(r.services, key)
}

func (r *ServiceRegistry) GetAll(serviceType string) []ServiceInfo {
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	var result []ServiceInfo
	for key, service := range r.services {
		normalizedType := strings.TrimSuffix(service.Type, ".local.")
		normalizedSearchType := strings.TrimSuffix(serviceType, ".local.")

		if normalizedType == normalizedSearchType {
			log.Printf("Found matching service: %s", key)
			result = append(result, service)
		}
	}

	log.Printf("Registry contains %d services, found %d of type %s",
		len(r.services), len(result), serviceType)
	return result
}

func (r *ServiceRegistry) CleanupExpired(maxAge time.Duration) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	now := time.Now()
	for key, service := range r.services {
		if now.Sub(service.Timestamp) > maxAge {
			delete(r.services, key)
			log.Printf("Removed expired service: %s", key)
		}
	}
}

func RegisterService(ctx context.Context, name, serviceType string, port int, metadata map[string]string) (*zeroconf.Server, error) {
	if !strings.HasSuffix(serviceType, ".local.") {
		if !strings.HasPrefix(serviceType, "_") {
			serviceType = "_" + serviceType
		}

		if !strings.Contains(serviceType, "._") {
			serviceType = strings.Replace(serviceType, "_", "._", 1)
		}

		serviceType += ".local."
	}

	var txtRecords []string
	for k, v := range metadata {
		txtRecords = append(txtRecords, fmt.Sprintf("%s=%s", k, v))
	}

	log.Printf("Registering service with name=%s, type=%s, port=%d", name, serviceType, port)

	server, err := zeroconf.Register(
		name,
		serviceType,
		"local.",
		port,
		txtRecords,
		nil,
	)

	if err != nil {
		return nil, fmt.Errorf("failed to register service: %w", err)
	}

	log.Printf("Successfully registered service '%s' of type '%s' on port %d", name, serviceType, port)
	return server, nil
}

func DiscoverServices(ctx context.Context, serviceType string, registry *ServiceRegistry) error {
	originalType := serviceType
	serviceType = normalizeServiceType(serviceType)
	log.Printf("Discovering services of type: %s (normalized from %s)", serviceType, originalType)

	entriesCh := make(chan *zeroconf.ServiceEntry)

	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		return fmt.Errorf("failed to create resolver: %w", err)
	}

	err = resolver.Browse(ctx, serviceType, "local.", entriesCh)
	if err != nil {
		return fmt.Errorf("failed to browse for services: %w", err)
	}

	go func() {
		for entry := range entriesCh {
			metadata := make(map[string]string)
			for _, txt := range entry.Text {
				parts := strings.SplitN(txt, "=", 2)
				if len(parts) == 2 {
					metadata[parts[0]] = parts[1]
				}
			}

			service := ServiceInfo{
				Name:      entry.Instance,
				Type:      originalType,
				Domain:    entry.Domain,
				Host:      entry.HostName,
				IPv4:      entry.AddrIPv4,
				IPv6:      entry.AddrIPv6,
				Port:      entry.Port,
				TTL:       entry.TTL,
				Timestamp: time.Now(),
				Metadata:  metadata,
			}

			registry.AddOrUpdate(service)
			log.Printf("Discovered service: %s.%s on %v:%d",
				service.Name, service.Type, service.IPv4, service.Port)
		}
	}()

	return nil
}

func normalizeServiceType(serviceType string) string {
	if strings.HasSuffix(serviceType, ".local.") {
		return serviceType
	}

	serviceType = strings.TrimSuffix(serviceType, ".local")

	if !strings.HasPrefix(serviceType, "_") {
		serviceType = "_" + serviceType
	}

	if !strings.Contains(serviceType, "._") {
		serviceType += "._tcp"
	}

	serviceType += ".local."

	return serviceType
}

func main() {
	var (
		registerMode = flag.Bool("register", false, "Register a service")
		discoverMode = flag.Bool("discover", false, "Discover services")
		once         = flag.Bool("once", false, "Run discovery once and exit")
		name         = flag.String("name", "my-service", "Service name")
		serviceType  = flag.String("type", "http", "Service type (e.g., http, ssh, ftp)")
		port         = flag.Int("port", 8080, "Port")
		metadata     = flag.String("meta", "version=1.0,api=rest", "Metadata as key=value,key2=value2")
	)
	flag.Parse()

	if !*registerMode && !*discoverMode {
		log.Fatal("Either -register or -discover mode must be specified")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	registry := NewServiceRegistry()

	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-signalCh
		log.Println("Shutting down...")
		cancel()
		os.Exit(0)
	}()

	metaMap := make(map[string]string)
	if *metadata != "" {
		for _, pair := range strings.Split(*metadata, ",") {
			parts := strings.SplitN(pair, "=", 2)
			if len(parts) == 2 {
				metaMap[parts[0]] = parts[1]
			}
		}
	}

	var server *zeroconf.Server
	if *registerMode {
		var err error
		server, err = RegisterService(ctx, *name, *serviceType, *port, metaMap)
		if err != nil {
			log.Fatalf("Error registering service: %v", err)
		}
		defer server.Shutdown()

		log.Printf("Service registered. Press Ctrl+C to shut down.")

		if *once {
			time.Sleep(5 * time.Second)
			log.Println("Registration complete. Service will no longer be discoverable.")
			return
		}
	}

	if *discoverMode {
		err := DiscoverServices(ctx, *serviceType, registry)
		if err != nil {
			log.Fatalf("Error discovering services: %v", err)
		}

		if *once {
			log.Printf("Discovering services for 5 seconds...")
			time.Sleep(5 * time.Second)

			services := registry.GetAll(*serviceType)
			fmt.Printf("\n--- Discovered %d services of type %s ---\n", len(services), *serviceType)
			for _, service := range services {
				var addrs []string
				for _, ip := range service.IPv4 {
					addrs = append(addrs, ip.String())
				}
				fmt.Printf("Service: %s\n", service.Name)
				fmt.Printf("  Addresses: %s\n", strings.Join(addrs, ", "))
				fmt.Printf("  Port: %d\n", service.Port)
				fmt.Printf("  Metadata: %v\n", service.Metadata)
				fmt.Println()
			}
			return
		}

		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()

			for {
				select {
				case <-ticker.C:
					registry.CleanupExpired(1 * time.Minute)
				case <-ctx.Done():
					return
				}
			}
		}()

		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		log.Printf("Listening for services. Press Ctrl+C to exit.")

		for {
			select {
			case <-ticker.C:
				services := registry.GetAll(*serviceType)
				fmt.Printf("\n--- Discovered %d services of type %s ---\n", len(services), *serviceType)
				for _, service := range services {
					var addrs []string
					for _, ip := range service.IPv4 {
						addrs = append(addrs, ip.String())
					}
					fmt.Printf("Service: %s\n", service.Name)
					fmt.Printf("  Addresses: %s\n", strings.Join(addrs, ", "))
					fmt.Printf("  Port: %d\n", service.Port)
					fmt.Printf("  Metadata: %v\n", service.Metadata)
					fmt.Println()
				}
			case <-ctx.Done():
				return
			}
		}
	} else {
		<-ctx.Done()
	}
}
