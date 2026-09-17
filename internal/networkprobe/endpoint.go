package networkprobe

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dont/shared"
)

const (
	endpointProbePrefix = "DST-ADMIN-ENDPOINT-PROBE-V1 "
	endpointAckPrefix   = "DST-ADMIN-ENDPOINT-ACK-V1 "
	maximumProbePacket  = 256
)

// ListenEndpoint temporarily answers authenticated UDP reachability probes.
// It is intended for a stopped DST Master so the real shard port is tested.
func ListenEndpoint(ctx context.Context, bindAddress string, port int, tokens []string, timeout time.Duration) ([]string, error) {
	listenConfig := net.ListenConfig{}
	packet, err := listenConfig.ListenPacket(ctx, "udp", net.JoinHostPort(strings.TrimSpace(bindAddress), strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	defer packet.Close()

	allowed := make(map[string]bool, len(tokens))
	for _, token := range tokens {
		allowed[token] = true
	}
	received := make(map[string]bool, len(tokens))
	deadline := time.Now().Add(timeout)
	buffer := make([]byte, maximumProbePacket)
	for len(received) < len(allowed) {
		if err := packet.SetReadDeadline(deadline); err != nil {
			return sortedTokenSet(received), err
		}
		count, address, readErr := packet.ReadFrom(buffer)
		if readErr != nil {
			var networkErr net.Error
			if errors.As(readErr, &networkErr) && networkErr.Timeout() {
				return sortedTokenSet(received), nil
			}
			if ctx.Err() != nil {
				return sortedTokenSet(received), ctx.Err()
			}
			return sortedTokenSet(received), readErr
		}
		token := strings.TrimPrefix(string(buffer[:count]), endpointProbePrefix)
		if token == string(buffer[:count]) || !allowed[token] {
			continue
		}
		ack := []byte(endpointAckPrefix + token)
		if len(ack) > maximumProbePacket {
			continue
		}
		if _, writeErr := packet.WriteTo(ack, address); writeErr != nil {
			continue
		}
		received[token] = true
	}
	return sortedTokenSet(received), nil
}

func ProbeEndpoints(ctx context.Context, endpoints []shared.RuntimeNetworkEndpointRequest, timeout time.Duration) []shared.RuntimeNetworkEndpointResult {
	results := make([]shared.RuntimeNetworkEndpointResult, len(endpoints))
	var wait sync.WaitGroup
	for index, endpoint := range endpoints {
		index, endpoint := index, endpoint
		wait.Add(1)
		go func() {
			defer wait.Done()
			results[index] = probeEndpoint(ctx, endpoint, timeout)
		}()
	}
	wait.Wait()
	return results
}

func probeEndpoint(ctx context.Context, endpoint shared.RuntimeNetworkEndpointRequest, timeout time.Duration) shared.RuntimeNetworkEndpointResult {
	result := shared.RuntimeNetworkEndpointResult{Address: endpoint.Address, Port: endpoint.Port}
	probeContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	connection, err := (&net.Dialer{}).DialContext(probeContext, "udp", net.JoinHostPort(endpoint.Address, strconv.Itoa(endpoint.Port)))
	if err != nil {
		result.Error = err.Error()
		return result
	}
	defer connection.Close()

	request := []byte(endpointProbePrefix + endpoint.Token)
	expected := []byte(endpointAckPrefix + endpoint.Token)
	buffer := make([]byte, maximumProbePacket)
	for probeContext.Err() == nil {
		sentAt := time.Now()
		if err := connection.SetDeadline(minimumDeadline(sentAt.Add(350*time.Millisecond), sentAt.Add(timeout))); err != nil {
			result.Error = err.Error()
			return result
		}
		if _, err := connection.Write(request); err != nil {
			result.Error = err.Error()
			return result
		}
		count, readErr := connection.Read(buffer)
		if readErr == nil && bytes.Equal(buffer[:count], expected) {
			result.Reachable = true
			result.LatencyMillis = max(time.Since(sentAt).Milliseconds(), 1)
			return result
		}
		if readErr != nil {
			var networkErr net.Error
			if !errors.As(readErr, &networkErr) || !networkErr.Timeout() {
				result.Error = readErr.Error()
				return result
			}
		}
	}
	result.Error = "endpoint probe timed out"
	return result
}

func sortedTokenSet(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func minimumDeadline(left, right time.Time) time.Time {
	if left.Before(right) {
		return left
	}
	return right
}
