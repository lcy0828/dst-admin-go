package httpapi

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/gin-gonic/gin"
)

// ConfigureTrustedProxies accepts only explicit proxy addresses or CIDRs.
// An empty list ignores forwarded client IP headers, including on direct access.
func ConfigureTrustedProxies(router *gin.Engine, configured string) error {
	var proxies []string
	for _, value := range strings.FieldsFunc(configured, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' }) {
		if _, err := netip.ParseAddr(value); err != nil {
			prefix, prefixErr := netip.ParsePrefix(value)
			if prefixErr != nil || prefix.Bits() == 0 {
				return fmt.Errorf("DST_ADMIN_TRUSTED_PROXIES requires explicit proxy IPs or CIDRs: %q", value)
			}
		}
		proxies = append(proxies, value)
	}
	return router.SetTrustedProxies(proxies)
}
