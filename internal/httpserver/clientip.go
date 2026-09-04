package httpserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

const (
	// maxForwardedForBytes bounds work and memory used to parse one forwarded
	// chain. Reverse proxies should never need a client chain this large.
	maxForwardedForBytes = 4096
	// maxForwardedForHops prevents comma-heavy headers from creating
	// unbounded parsing work even while the byte limit is respected.
	maxForwardedForHops = 32
)

type clientAddressContextKeyType struct{}

var clientAddressContextKey clientAddressContextKeyType

// NewTrustedClientAddressMiddleware parses the configured proxy CIDRs once
// and returns middleware that records the verified client address in each
// request context. X-Forwarded-For is considered only when RemoteAddr belongs
// to a configured proxy. From there, the chain is walked right-to-left past
// trusted proxies to the first untrusted client.
func NewTrustedClientAddressMiddleware(trustedProxyCIDRs []string) (func(http.Handler) http.Handler, error) {
	trusted := make([]netip.Prefix, len(trustedProxyCIDRs))
	for i, cidr := range trustedProxyCIDRs {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			return nil, fmt.Errorf("httpserver: invalid trusted proxy CIDR %q: %w", cidr, err)
		}
		trusted[i] = prefix.Masked()
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			peer, err := parseRemoteAddress(r.RemoteAddr)
			if err != nil {
				writeAPIError(w, http.StatusBadRequest, "invalid client address")
				return
			}

			client := peer
			if isTrustedProxy(peer, trusted) {
				values := r.Header.Values("X-Forwarded-For")
				switch len(values) {
				case 0:
				case 1:
					client, err = forwardedClientAddress(values[0], trusted)
					if err != nil {
						writeAPIError(w, http.StatusBadRequest, "invalid forwarded client address")
						return
					}
				default:
					writeAPIError(w, http.StatusBadRequest, "invalid forwarded client address")
					return
				}
			}

			ctx := context.WithValue(r.Context(), clientAddressContextKey, client)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}, nil
}

func forwardedClientAddress(value string, trusted []netip.Prefix) (netip.Addr, error) {
	if value == "" || len(value) > maxForwardedForBytes {
		return netip.Addr{}, errors.New("forwarded chain is empty or too large")
	}

	var client netip.Addr
	hops := 0
	for end := len(value); ; {
		hops++
		if hops > maxForwardedForHops {
			return netip.Addr{}, errors.New("forwarded chain has too many hops")
		}
		comma := strings.LastIndexByte(value[:end], ',')
		part := strings.TrimSpace(value[comma+1 : end])
		if part == "" {
			return netip.Addr{}, errors.New("forwarded chain contains an empty hop")
		}
		addr, err := netip.ParseAddr(part)
		if err != nil || addr.Zone() != "" {
			return netip.Addr{}, errors.New("forwarded chain contains an invalid address")
		}
		addr = addr.Unmap()
		if !client.IsValid() && !isTrustedProxy(addr, trusted) {
			client = addr
		}
		if comma < 0 {
			break
		}
		end = comma
	}
	if !client.IsValid() {
		return netip.Addr{}, errors.New("forwarded chain contains no client address")
	}
	return client, nil
}

func parseRemoteAddress(remoteAddr string) (netip.Addr, error) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	if host == "" {
		return netip.Addr{}, errors.New("remote address is empty")
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("parse remote address: %w", err)
	}
	return addr.Unmap(), nil
}

func isTrustedProxy(addr netip.Addr, trusted []netip.Prefix) bool {
	for _, prefix := range trusted {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func clientAddressFromContext(ctx context.Context) (netip.Addr, bool) {
	addr, ok := ctx.Value(clientAddressContextKey).(netip.Addr)
	return addr, ok && addr.IsValid()
}

func canonicalRemoteAddress(remoteAddr string) string {
	if addr, err := parseRemoteAddress(remoteAddr); err == nil {
		return addr.String()
	}

	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil && host != "" {
		return host
	}
	if remoteAddr != "" {
		return remoteAddr
	}
	return "unknown"
}
