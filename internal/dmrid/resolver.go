package dmrid

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Resolver struct {
	byID       map[uint32]string
	byCallsign map[string]uint32
}

func Load(path string) (*Resolver, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	resolver := &Resolver{
		byID:       map[uint32]string{},
		byCallsign: map[string]uint32{},
	}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		id, callsign, ok := parseLine(line)
		if !ok {
			continue
		}
		resolver.byID[id] = callsign
		normalized := NormalizeCallsign(callsign)
		if normalized != "" {
			if _, exists := resolver.byCallsign[normalized]; !exists {
				resolver.byCallsign[normalized] = id
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan dmrid file: %w", err)
	}
	return resolver, nil
}

func (r *Resolver) Lookup(id uint32) string {
	if r == nil || id == 0 {
		return ""
	}
	return r.byID[id]
}

func (r *Resolver) LookupID(callsign string) uint32 {
	if r == nil {
		return 0
	}
	return r.byCallsign[NormalizeCallsign(callsign)]
}

func NormalizeCallsign(value string) string {
	var out strings.Builder
	out.Grow(len(value))
	for _, ch := range strings.ToUpper(strings.TrimSpace(value)) {
		if ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' {
			out.WriteRune(ch)
		}
	}
	return out.String()
}

func IsValidCallsign(value string) bool {
	callsign := NormalizeCallsign(value)
	if len(callsign) < 4 || len(callsign) > 6 {
		return false
	}
	digits := 0
	letters := 0
	for _, ch := range callsign {
		if ch >= '0' && ch <= '9' {
			digits++
			continue
		}
		if ch >= 'A' && ch <= 'Z' {
			letters++
		}
	}
	return digits == 1 && letters+digits == len(callsign)
}

func parseLine(line string) (uint32, string, bool) {
	delimiter := ","
	if strings.Contains(line, ";") {
		delimiter = ";"
	}
	parts := strings.Split(line, delimiter)
	if len(parts) < 2 {
		return 0, "", false
	}
	id64, err := strconv.ParseUint(strings.TrimSpace(parts[0]), 10, 32)
	if err != nil {
		return 0, "", false
	}
	callsign := strings.TrimSpace(parts[1])
	if callsign == "" {
		return 0, "", false
	}
	return uint32(id64), callsign, true
}
