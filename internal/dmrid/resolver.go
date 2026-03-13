package dmrid

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Resolver struct {
	byID map[uint32]string
}

func Load(path string) (*Resolver, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	resolver := &Resolver{byID: map[uint32]string{}}
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
