package discovery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/hashicorp/mdns"

	"github.com/EJM-Digital/Xuva/server/internal/config"
)

const (
	DefaultServiceType = "_xuva._tcp"
	serviceDomain      = "local."
	bootstrapAPIPath   = "/api/client/bootstrap"
)

type advertisedAddressFamily int

const (
	addressFamilyAny advertisedAddressFamily = iota
	addressFamilyIPv4
	addressFamilyIPv6
)

var virtualInterfaceTerms = []string{
	"vethernet",
	"docker",
	"wsl",
	"hyper-v",
	"hyperv",
	"vmware",
	"virtualbox",
	"tailscale",
	"zerotier",
}

type Status struct {
	Enabled      bool     `json:"enabled"`
	Running      bool     `json:"running"`
	ServiceName  string   `json:"serviceName,omitempty"`
	ServiceType  string   `json:"serviceType,omitempty"`
	HostName     string   `json:"hostName,omitempty"`
	WebURL       string   `json:"webUrl,omitempty"`
	Port         int      `json:"port,omitempty"`
	Interfaces   []string `json:"interfaces,omitempty"`
	AdvertiseIPs []string `json:"advertiseIps,omitempty"`
	TXTRecords   []string `json:"txtRecords,omitempty"`
	LastError    string   `json:"lastError,omitempty"`
	Note         string   `json:"note,omitempty"`
}

type AdvertiseConfig struct {
	ServiceName string
	ServiceType string
	HostName    string
	Port        int
	IPs         []net.IP
	TXTRecords  []string
}

type announcer interface {
	Shutdown() error
}

type announcerFactory func(AdvertiseConfig) (announcer, error)

type compositeAnnouncer []announcer

func (a compositeAnnouncer) Shutdown() error {
	var failures []string
	for _, item := range a {
		if item == nil {
			continue
		}
		if err := item.Shutdown(); err != nil {
			failures = append(failures, err.Error())
		}
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

type interfaceAdvertisement struct {
	Iface net.Interface
	IPs   []net.IP
}

type Service struct {
	cfg     config.Config
	factory announcerFactory

	mu        sync.RWMutex
	announcer announcer
	status    Status
	stopOnce  sync.Once
}

func NewService(cfg config.Config) *Service {
	return &Service{
		cfg:     cfg,
		factory: newMDNSAnnouncer,
		status: Status{
			Enabled:     cfg.DiscoveryEnabled,
			ServiceName: displayServiceName(cfg.ServerName),
			ServiceType: fullServiceType(cfg.DiscoveryServiceType),
		},
	}
}

func NewServiceForTest(cfg config.Config, factory func(AdvertiseConfig) (announcer, error)) *Service {
	service := NewService(cfg)
	service.factory = factory
	return service
}

func (s *Service) Start(ctx context.Context) {
	status := Status{
		Enabled:     s.cfg.DiscoveryEnabled,
		ServiceName: displayServiceName(s.cfg.ServerName),
		ServiceType: fullServiceType(s.cfg.DiscoveryServiceType),
	}
	if !s.cfg.DiscoveryEnabled {
		status.Note = "Local discovery is turned off."
		s.setStatus(status)
		slog.Info("Local discovery disabled")
		return
	}

	port, err := httpPort(s.cfg.HTTPAddr)
	if err != nil {
		status.LastError = "http port is unavailable"
		status.Note = "Local discovery is not running."
		s.setStatus(status)
		slog.Warn("Local discovery could not start: http port is unavailable")
		return
	}
	status.Port = port

	if config.HTTPAddrLoopbackOnly(s.cfg.HTTPAddr) {
		status.Note = "This server is listening only on this device right now."
		s.setStatus(status)
		slog.Info("Local discovery disabled", "reason", "server is listening only on loopback")
		return
	}

	ips, err := advertisedIPs(s.cfg.HTTPAddr)
	if err != nil {
		status.LastError = err.Error()
		status.Note = "Local discovery is not running."
		s.setStatus(status)
		slog.Warn("Local discovery could not start: " + err.Error())
		return
	}
	status.AdvertiseIPs = ipStrings(ips)
	status.Interfaces = interfaceNamesForIPs(ips)
	status.WebURL = discoveryWebURL(s.cfg, status.HostName, port, ips)
	status.HostName = discoveryConnectionHost(status.WebURL)

	txtRecords := []string{
		"app=xuva",
		"api=" + bootstrapAPIPath,
		"serverName=" + status.ServiceName,
	}
	if status.HostName != "" {
		txtRecords = append(txtRecords, "hostName="+status.HostName)
	}
	if status.WebURL != "" {
		txtRecords = append(txtRecords, "web="+status.WebURL)
	}
	sort.Strings(txtRecords)
	status.TXTRecords = append([]string(nil), txtRecords...)

	instance := AdvertiseConfig{
		ServiceName: status.ServiceName,
		ServiceType: normalizedServiceType(s.cfg.DiscoveryServiceType),
		HostName:    discoveryHostRecord(status.ServiceName),
		Port:        port,
		IPs:         ips,
		TXTRecords:  txtRecords,
	}
	announcer, err := s.factory(instance)
	if err != nil {
		status.LastError = err.Error()
		status.Note = "Local discovery is not running."
		s.setStatus(status)
		slog.Warn("Local discovery could not start: " + err.Error())
		return
	}

	status.Running = true
	s.mu.Lock()
	s.announcer = announcer
	s.status = status
	s.mu.Unlock()
	slog.Info("Local discovery started as " + status.ServiceName)

	go func() {
		<-ctx.Done()
		s.Shutdown()
	}()
}

func (s *Service) Shutdown() {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		announcer := s.announcer
		s.announcer = nil
		if s.status.Running {
			s.status.Running = false
			if s.status.Note == "" {
				s.status.Note = "Local discovery is not running."
			}
		}
		s.mu.Unlock()

		if announcer != nil {
			if err := announcer.Shutdown(); err != nil {
				slog.Warn("Local discovery could not stop cleanly: " + err.Error())
			} else {
				slog.Info("Local discovery stopped")
			}
		}
	})
}

func (s *Service) Status() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	status := s.status
	if len(status.Interfaces) > 0 {
		status.Interfaces = append([]string(nil), status.Interfaces...)
	}
	if len(status.AdvertiseIPs) > 0 {
		status.AdvertiseIPs = append([]string(nil), status.AdvertiseIPs...)
	}
	if len(status.TXTRecords) > 0 {
		status.TXTRecords = append([]string(nil), status.TXTRecords...)
	}
	return status
}

func (s *Service) setStatus(status Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
}

func newMDNSAnnouncer(cfg AdvertiseConfig) (announcer, error) {
	interfaces := advertisementInterfacesForIPs(cfg.IPs)
	if len(interfaces) == 0 {
		return newMDNSServer(cfg, nil, cfg.IPs)
	}

	var servers compositeAnnouncer
	var failures []string
	for _, candidate := range interfaces {
		iface := candidate.Iface
		server, err := newMDNSServer(cfg, &iface, candidate.IPs)
		if err != nil {
			failures = append(failures, iface.Name+": "+err.Error())
			continue
		}
		slog.Info("Local discovery advertised on interface", "name", iface.Name, "ips", ipStrings(candidate.IPs))
		servers = append(servers, server)
	}
	if len(servers) == 0 {
		if len(failures) > 0 {
			return nil, errors.New(strings.Join(failures, "; "))
		}
		return nil, errors.New("no multicast-capable network interface is available")
	}
	if len(failures) > 0 {
		slog.Warn("Local discovery could not start on every interface", "errors", strings.Join(failures, "; "))
	}
	return servers, nil
}

func newMDNSServer(cfg AdvertiseConfig, iface *net.Interface, ips []net.IP) (announcer, error) {
	service, err := mdns.NewMDNSService(
		cfg.ServiceName,
		cfg.ServiceType,
		serviceDomain,
		cfg.HostName,
		cfg.Port,
		ips,
		cfg.TXTRecords,
	)
	if err != nil {
		return nil, err
	}
	return mdns.NewServer(&mdns.Config{Zone: service, Iface: iface})
}

func advertisementInterfacesForIPs(ips []net.IP) []interfaceAdvertisement {
	if len(ips) == 0 {
		return nil
	}
	wanted := map[string]bool{}
	for _, ip := range ips {
		if ip != nil {
			wanted[ip.String()] = true
		}
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var output []interfaceAdvertisement
	for i := range interfaces {
		iface := interfaces[i]
		if !usableMulticastInterface(iface) {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		var matched []net.IP
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if usableAdvertiseIP(ip) && wanted[ip.String()] {
				matched = append(matched, ip)
			}
		}
		if len(matched) > 0 {
			output = append(output, interfaceAdvertisement{Iface: iface, IPs: dedupeIPs(matched)})
		}
	}
	sort.SliceStable(output, func(i int, j int) bool {
		return interfacePreference(output[i].Iface.Name) < interfacePreference(output[j].Iface.Name)
	})
	return output
}

func interfaceNamesForIPs(ips []net.IP) []string {
	advertisements := advertisementInterfacesForIPs(ips)
	names := make([]string, 0, len(advertisements))
	for _, item := range advertisements {
		names = append(names, item.Iface.Name)
	}
	return names
}

func usableMulticastInterface(iface net.Interface) bool {
	if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
		return false
	}
	return iface.Flags&net.FlagMulticast != 0
}

func usableAdvertiseIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() {
		return false
	}
	return true
}

func interfacePreference(name string) int {
	if isVirtualInterfaceName(name) {
		return 10
	}
	lower := strings.ToLower(name)
	if strings.Contains(lower, "bluetooth") {
		return 20
	}
	return 0
}

func isVirtualInterfaceName(name string) bool {
	lower := strings.ToLower(name)
	for _, term := range virtualInterfaceTerms {
		if strings.Contains(lower, term) {
			return true
		}
	}
	return false
}

func displayServiceName(value string) string {
	normalized, err := config.NormalizeServerName(value)
	if err != nil {
		return "Xuva"
	}
	return normalized
}

func discoveryHostRecord(serviceName string) string {
	return discoveryHostLabel(serviceName) + ".local."
}

func discoveryHostLabel(serviceName string) string {
	source := strings.ToLower(strings.TrimSpace(serviceName))
	var builder strings.Builder
	lastWasDash := false
	for _, char := range source {
		isAlphaNum := (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9')
		if isAlphaNum {
			builder.WriteRune(char)
			lastWasDash = false
			continue
		}
		if !lastWasDash && builder.Len() > 0 {
			builder.WriteByte('-')
			lastWasDash = true
		}
	}
	label := strings.Trim(builder.String(), "-")
	if label == "" {
		label = "xuva"
	}
	if label != "xuva" && !strings.Contains(label, "xuva") {
		label += "-xuva"
	}
	if len(label) > 63 {
		label = strings.Trim(label[:63], "-")
	}
	if label == "" {
		return "xuva"
	}
	return label
}

func discoveryWebURL(cfg config.Config, hostName string, port int, ips []net.IP) string {
	if webOrigin, err := config.NormalizeWebOrigin(cfg.CanonicalWebOrigin); err == nil && webOrigin != "" {
		return webOrigin
	}
	if ip := preferredWebIP(ips); ip != "" {
		return "http://" + ip + ":" + strconv.Itoa(port)
	}
	if strings.TrimSpace(hostName) == "" || port <= 0 {
		return ""
	}
	return "http://" + hostName + ":" + strconv.Itoa(port)
}

func discoveryConnectionHost(webURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(webURL))
	if err != nil {
		return ""
	}
	host := strings.TrimSpace(parsed.Hostname())
	if host == "" {
		return ""
	}
	return host
}

func preferredWebIP(ips []net.IP) string {
	for _, item := range advertisementInterfacesForIPs(ips) {
		for _, ip := range item.IPs {
			if ip == nil || ip.To4() == nil {
				continue
			}
			if !usableAdvertiseIP(ip) {
				continue
			}
			return ip.String()
		}
	}
	for _, item := range advertisementInterfacesForIPs(ips) {
		for _, ip := range item.IPs {
			if ip != nil && usableAdvertiseIP(ip) {
				return "[" + ip.String() + "]"
			}
		}
	}
	for _, ip := range ips {
		if ip == nil || ip.To4() == nil {
			continue
		}
		if usableAdvertiseIP(ip) {
			return ip.String()
		}
	}
	for _, ip := range ips {
		if ip != nil && usableAdvertiseIP(ip) {
			return "[" + ip.String() + "]"
		}
	}
	return ""
}

func normalizedServiceType(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return DefaultServiceType
	}
	trimmed = strings.TrimSuffix(trimmed, ".local.")
	trimmed = strings.TrimSuffix(trimmed, ".local")
	return trimmed
}

func fullServiceType(value string) string {
	return normalizedServiceType(value) + ".local."
}

func httpPort(httpAddr string) (int, error) {
	host := strings.TrimSpace(httpAddr)
	if host == "" {
		return 0, errors.New("http port is unavailable")
	}
	_, port, err := net.SplitHostPort(host)
	if err != nil {
		return 0, errors.New("http port is unavailable")
	}
	parsed, err := strconv.Atoi(port)
	if err != nil || parsed < 1 || parsed > 65535 {
		return 0, errors.New("http port is unavailable")
	}
	return parsed, nil
}

func advertisedIPs(httpAddr string) ([]net.IP, error) {
	host, _, err := net.SplitHostPort(strings.TrimSpace(httpAddr))
	if err != nil {
		return nil, errors.New("http address is invalid")
	}
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	family := listenerAddressFamily(host)
	switch {
	case host == "", host == "0.0.0.0", host == "::":
		ips, err := activeLANIPs()
		if err != nil {
			return nil, err
		}
		filtered := filterAdvertisedIPsByFamily(ips, family)
		if len(filtered) == 0 {
			return nil, errors.New("no local network address is available")
		}
		return filtered, nil
	case strings.EqualFold(host, "localhost"):
		return nil, errors.New("server is listening only on this device")
	}

	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() {
			return nil, errors.New("server is listening only on this device")
		}
		return []net.IP{ip}, nil
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, fmt.Errorf("could not resolve advertised address")
	}
	filtered := make([]net.IP, 0, len(ips))
	for _, ip := range ips {
		if ip == nil || ip.IsLoopback() {
			continue
		}
		filtered = append(filtered, ip)
	}
	filtered = filterAdvertisedIPsByFamily(filtered, family)
	if len(filtered) == 0 {
		return nil, errors.New("no local network address is available")
	}
	return dedupeIPs(filtered), nil
}

func activeLANIPs() ([]net.IP, error) {
	// Advertise on EVERY multicast-capable interface — physical and virtual.
	// The earlier behaviour preferred physical-only and only fell back to
	// virtual when no physical NIC had an IP, but that suppressed reachable
	// paths when a host had both kinds (e.g. a Tailscale interface that's
	// the only route between server and Apple TV, or a Hyper-V Default
	// Switch that the client is bridged through). The downstream
	// advertisementInterfacesForIPs still sorts with physical-first via
	// interfacePreference so clients try LAN before overlay networks; we
	// just don't drop the overlay entries from the list any more.
	output := collectLANIPs(true)
	if len(output) == 0 {
		return nil, errors.New("no local network address is available")
	}
	return output, nil
}

func collectLANIPs(includeVirtual bool) []net.IP {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var output []net.IP
	for _, iface := range interfaces {
		if !usableMulticastInterface(iface) {
			continue
		}
		if !includeVirtual && isVirtualInterfaceName(iface.Name) {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch value := addr.(type) {
			case *net.IPNet:
				ip = value.IP
			case *net.IPAddr:
				ip = value.IP
			}
			if !usableAdvertiseIP(ip) {
				continue
			}
			output = append(output, ip)
		}
	}
	return dedupeIPs(output)
}

func listenerAddressFamily(host string) advertisedAddressFamily {
	trimmed := strings.TrimSpace(strings.Trim(host, "[]"))
	switch {
	case trimmed == "", trimmed == "0.0.0.0":
		return addressFamilyIPv4
	case trimmed == "::":
		return addressFamilyIPv6
	}
	ip := net.ParseIP(trimmed)
	if ip == nil {
		return addressFamilyAny
	}
	if ip.To4() != nil {
		return addressFamilyIPv4
	}
	return addressFamilyIPv6
}

func filterAdvertisedIPsByFamily(ips []net.IP, family advertisedAddressFamily) []net.IP {
	if family == addressFamilyAny {
		return dedupeIPs(ips)
	}
	filtered := make([]net.IP, 0, len(ips))
	for _, ip := range ips {
		if ip == nil {
			continue
		}
		isIPv4 := ip.To4() != nil
		if family == addressFamilyIPv4 && isIPv4 {
			filtered = append(filtered, ip)
		}
		if family == addressFamilyIPv6 && !isIPv4 {
			filtered = append(filtered, ip)
		}
	}
	return dedupeIPs(filtered)
}

func dedupeIPs(values []net.IP) []net.IP {
	seen := map[string]bool{}
	output := make([]net.IP, 0, len(values))
	for _, ip := range values {
		if ip == nil {
			continue
		}
		normalized := ip.String()
		if normalized == "" || seen[normalized] {
			continue
		}
		seen[normalized] = true
		output = append(output, ip)
	}
	sort.Slice(output, func(i int, j int) bool {
		return output[i].String() < output[j].String()
	})
	return output
}

func ipStrings(values []net.IP) []string {
	output := make([]string, 0, len(values))
	for _, ip := range values {
		if ip != nil && ip.String() != "" {
			output = append(output, ip.String())
		}
	}
	sort.Strings(output)
	return output
}
