package util

import "net"

// CanonicalHWAddr normalizes mac address spellings to the canonical colon
// form so lease and allocation identities do not depend on the formatting.
func CanonicalHWAddr(hwAddr string) string {
	if hw, parseErr := net.ParseMAC(hwAddr); parseErr == nil {
		return hw.String()
	}

	return hwAddr
}
