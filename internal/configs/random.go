package configs

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"net"
	"strings"
)

const pwHashAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789./"

// pseudoCiscoMD5 produces a string shaped like Cisco's "enable secret 5" MD5 form.
// It is not a real hash; the simulator never verifies it.
func pseudoCiscoMD5(rng *rand.Rand) string {
	var sb strings.Builder
	sb.WriteString("$1$")
	for i := 0; i < 4; i++ {
		sb.WriteByte(pwHashAlphabet[rng.Intn(len(pwHashAlphabet))])
	}
	sb.WriteByte('$')
	for i := 0; i < 22; i++ {
		sb.WriteByte(pwHashAlphabet[rng.Intn(len(pwHashAlphabet))])
	}
	return sb.String()
}

func snmpCommunity(rng *rand.Rand) string {
	const n = 10
	const alpha = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ"
	var sb strings.Builder
	sb.Grow(n)
	for i := 0; i < n; i++ {
		sb.WriteByte(alpha[rng.Intn(len(alpha))])
	}
	return sb.String()
}

// siteCode maps 0..17575 to a 3-letter lowercase string. Deterministic.
func siteCode(n int) string {
	n = n % (26 * 26 * 26)
	a := byte('a' + (n/26/26)%26)
	b := byte('a' + (n/26)%26)
	c := byte('a' + n%26)
	return string([]byte{a, b, c})
}

// serialFor derives a stable pseudo-serial (11 chars) from a hostname.
func serialFor(hostname string) string {
	sum := sha256.Sum256([]byte(hostname))
	hexed := hex.EncodeToString(sum[:])
	return "FOC" + strings.ToUpper(hexed[:8])
}

// randomRFC1918 returns a random RFC1918 host IP.
func randomRFC1918(rng *rand.Rand) string {
	switch rng.Intn(3) {
	case 0:
		return fmt.Sprintf("10.%d.%d.%d", rng.Intn(256), rng.Intn(256), 1+rng.Intn(254))
	case 1:
		return fmt.Sprintf("172.%d.%d.%d", 16+rng.Intn(16), rng.Intn(256), 1+rng.Intn(254))
	default:
		return fmt.Sprintf("192.168.%d.%d", rng.Intn(256), 1+rng.Intn(254))
	}
}

// randomPublicIP returns a random non-RFC1918 IPv4 host IP (BGP neighbor style).
func randomPublicIP(rng *rand.Rand) string {
	for {
		a := 1 + rng.Intn(222)
		if a == 10 || a == 127 || a == 172 || a == 192 {
			continue
		}
		return fmt.Sprintf("%d.%d.%d.%d", a, rng.Intn(256), rng.Intn(256), 1+rng.Intn(254))
	}
}

// randomMgmtIP draws from a typical OOB-mgmt 10.0.0.0/8 space.
func randomMgmtIP(rng *rand.Rand) string {
	return fmt.Sprintf("10.%d.%d.%d", rng.Intn(256), rng.Intn(256), 1+rng.Intn(254))
}

func randomIPs(rng *rand.Rand, n int, f func(*rand.Rand) string) []string {
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = f(rng)
	}
	return out
}

// ipPlusOffset increments an IPv4 address by n (last-octet-and-beyond).
func ipPlusOffset(base string, offset int) string {
	ip := net.ParseIP(base).To4()
	if ip == nil {
		return base
	}
	val := uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
	val += uint32(offset)
	return fmt.Sprintf("%d.%d.%d.%d", byte(val>>24), byte(val>>16), byte(val>>8), byte(val))
}

// wildcardFor returns the Cisco wildcard mask for a given prefix length.
func wildcardFor(prefixLen int) string {
	if prefixLen < 0 {
		prefixLen = 0
	}
	if prefixLen > 32 {
		prefixLen = 32
	}
	mask := uint32(0xFFFFFFFF) >> prefixLen
	return fmt.Sprintf("%d.%d.%d.%d", byte(mask>>24), byte(mask>>16), byte(mask>>8), byte(mask))
}

// netmaskFor returns the dotted-decimal netmask for a prefix length.
func netmaskFor(prefixLen int) string {
	if prefixLen < 0 {
		prefixLen = 0
	}
	if prefixLen > 32 {
		prefixLen = 32
	}
	mask := uint32(0xFFFFFFFF) << (32 - prefixLen)
	return fmt.Sprintf("%d.%d.%d.%d", byte(mask>>24), byte(mask>>16), byte(mask>>8), byte(mask))
}

// deviceRand builds a reproducible RNG for a given device index.
func deviceRand(seed int64, index int) *rand.Rand {
	// Mixing constants from Knuth multiplicative hashing. Two mixes so neighbouring
	// indices produce noticeably different streams.
	a := uint64(seed) ^ (uint64(index) * 2654435761)
	a ^= a >> 33
	a *= 0xff51afd7ed558ccd
	a ^= a >> 33
	a *= 0xc4ceb9fe1a85ec53
	a ^= a >> 33
	return rand.New(rand.NewSource(int64(a)))
}

func pickWord(rng *rand.Rand, list []string) string {
	return list[rng.Intn(len(list))]
}

var citySyllables = []string{
	"lax", "jfk", "sfo", "ord", "dfw", "atl", "sea", "mia", "bos", "iad",
	"sjc", "phx", "den", "pdx", "mci", "stl", "iah", "las", "slc", "mem",
	"cle", "mke", "msp", "dtw", "cmh", "cvg", "pit", "bwi", "dca", "phl",
	"rdu", "clt", "jax", "tpa", "bna", "oma", "buf", "bdl", "abq", "tus",
}

var siteFunctions = []string{
	"hq", "dc", "colo", "branch", "edge", "pop", "lab", "admin", "rnd", "ops",
}
