package networkprobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"dont/shared"
)

const maximumResponseBytes = 256

var globalProviders = []string{
	"https://api.ipify.org",
	"https://checkip.amazonaws.com",
	"https://ifconfig.me/ip",
}

var cnProviders = []string{
	"https://ip.3322.net",
	"https://myip.ipip.net",
	"https://whois.pconline.com.cn/ipJson.jsp?json=true",
}

var (
	ErrNoPublicAddress = errors.New("no public egress address was detected")
	cgnatPrefix        = netip.MustParsePrefix("100.64.0.0/10")
)

type Detector struct {
	Client          *http.Client
	Providers       []string
	RegionProviders map[shared.RuntimeNetworkRegion][]string
}

func Detect(ctx context.Context, region shared.RuntimeNetworkRegion) (netip.Addr, error) {
	return (Detector{}).Detect(ctx, region)
}

func (d Detector) Detect(ctx context.Context, region shared.RuntimeNetworkRegion) (netip.Addr, error) {
	client := d.Client
	if client == nil {
		client = &http.Client{Timeout: 4 * time.Second}
	}
	providers := d.Providers
	if len(providers) == 0 {
		providers = d.RegionProviders[region]
	}
	if len(providers) == 0 {
		switch region {
		case shared.RuntimeNetworkRegionCN:
			providers = cnProviders
		case shared.RuntimeNetworkRegionGlobal:
			providers = globalProviders
		default:
			return netip.Addr{}, errors.New("unsupported egress probe region")
		}
	}

	probeContext, cancel := context.WithCancel(ctx)
	defer cancel()
	type probeResult struct {
		address netip.Addr
		err     error
	}
	results := make(chan probeResult, len(providers))
	for _, provider := range providers {
		provider := provider
		go func() {
			address, err := requestAddress(probeContext, client, provider)
			results <- probeResult{address: address, err: err}
		}()
	}
	var failures []error
	for range providers {
		select {
		case <-ctx.Done():
			return netip.Addr{}, ctx.Err()
		case result := <-results:
			if result.err == nil {
				cancel()
				return result.address, nil
			}
			failures = append(failures, result.err)
		}
	}
	return netip.Addr{}, fmt.Errorf("%w: %v", ErrNoPublicAddress, errors.Join(failures...))
}

func ParsePublicAddress(value string) (netip.Addr, error) {
	address, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil || !address.IsValid() || !address.IsGlobalUnicast() || address.IsPrivate() ||
		address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsMulticast() || cgnatPrefix.Contains(address) {
		return netip.Addr{}, ErrNoPublicAddress
	}
	return address.Unmap(), nil
}

func requestAddress(ctx context.Context, client *http.Client, provider string) (netip.Addr, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, provider, nil)
	if err != nil {
		return netip.Addr{}, err
	}
	request.Header.Set("Accept", "text/plain")
	request.Header.Set("User-Agent", "dst-admin-egress-probe/1")
	response, err := client.Do(request)
	if err != nil {
		return netip.Addr{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return netip.Addr{}, fmt.Errorf("provider returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maximumResponseBytes+1))
	if err != nil {
		return netip.Addr{}, err
	}
	if len(body) > maximumResponseBytes {
		return netip.Addr{}, errors.New("provider response is too large")
	}
	return ParseProviderAddress(body)
}

var ipv4Pattern = regexp.MustCompile(`(?:[0-9]{1,3}\.){3}[0-9]{1,3}`)

func ParseProviderAddress(body []byte) (netip.Addr, error) {
	if address, err := ParsePublicAddress(string(body)); err == nil {
		return address, nil
	}
	var value struct {
		IP string `json:"ip"`
	}
	if json.Unmarshal(body, &value) == nil {
		if address, err := ParsePublicAddress(value.IP); err == nil {
			return address, nil
		}
	}
	for _, candidate := range ipv4Pattern.FindAllString(string(body), -1) {
		if address, err := ParsePublicAddress(candidate); err == nil {
			return address, nil
		}
	}
	return netip.Addr{}, ErrNoPublicAddress
}

func IsPublicIP(value string) bool {
	address := net.ParseIP(strings.TrimSpace(value))
	if address == nil {
		return false
	}
	_, err := ParsePublicAddress(address.String())
	return err == nil
}
