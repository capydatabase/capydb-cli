package source

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
)

// sourceEnvKeys are the env keys that name an app's database, most specific
// first: a direct URL is the one a dump should come from.
var sourceEnvKeys = []string{"DIRECT_URL", "DATABASE_DIRECT_URL", "DATABASE_URL"}

// EnvSourceURL returns the first database URL in an env file that points
// somewhere other than CapyDB - the database a new CapyDB project is likely
// to be imported from. A missing file is not an error.
func EnvSourceURL(path string) (key, value string, err error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", nil
		}
		return "", "", fmt.Errorf("read env file: %w", err)
	}
	defer func() { _ = file.Close() }()

	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		name, raw, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		values[strings.TrimSpace(name)] = strings.Trim(strings.TrimSpace(raw), `'"`)
	}
	if err := scanner.Err(); err != nil {
		return "", "", fmt.Errorf("read env file: %w", err)
	}
	for _, name := range sourceEnvKeys {
		candidate := values[name]
		if candidate == "" {
			continue
		}
		endpoint, parseErr := ParseEndpoint(candidate)
		if parseErr != nil || isCapyDBEndpoint(endpoint) {
			continue
		}
		return name, candidate, nil
	}
	return "", "", nil
}

func isCapyDBEndpoint(endpoint Endpoint) bool {
	for _, host := range endpoint.Hosts {
		if strings.HasSuffix(strings.ToLower(host), ".capydb.dev") {
			return true
		}
	}
	return false
}
