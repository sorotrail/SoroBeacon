package config

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

var yamlLineRe = regexp.MustCompile(`(?i)line\s+(\d+)`)

// loadFileValues reads the optional CONFIG_FILE as YAML. The file is layered
// beneath the environment: env vars win when set, otherwise the file fills in
// defaults so a checked-in config file stays safe while emergency overrides
// still work.
func loadFileValues() (map[string]string, error) {
	path := strings.TrimSpace(os.Getenv("CONFIG_FILE"))
	if path == "" {
		return nil, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("invalid CONFIG_FILE %q: could not read file: %w", path, err)
	}

	var values map[string]any
	if err := yaml.Unmarshal(raw, &values); err != nil {
		return nil, formatConfigFileYAMLError(path, err)
	}

	out := make(map[string]string)
	for key, value := range values {
		if key == "" {
			continue
		}
		if s, ok := stringifyConfigValue(value); ok {
			out[normalizeConfigKey(key)] = s
		}
	}
	return out, nil
}

func lookupConfigValue(key string, fileValues map[string]string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	if fileValues == nil {
		return ""
	}
	return fileValues[normalizeConfigKey(key)]
}

func normalizeConfigKey(key string) string {
	key = strings.TrimSpace(key)
	key = strings.ReplaceAll(key, "-", "_")
	key = strings.ReplaceAll(key, ".", "_")
	return strings.ToUpper(key)
}

func stringifyConfigValue(value any) (string, bool) {
	switch v := value.(type) {
	case nil:
		return "", false
	case string:
		return v, true
	case bool:
		return strconv.FormatBool(v), true
	case int:
		return strconv.Itoa(v), true
	case int8:
		return strconv.FormatInt(int64(v), 10), true
	case int16:
		return strconv.FormatInt(int64(v), 10), true
	case int32:
		return strconv.FormatInt(int64(v), 10), true
	case int64:
		return strconv.FormatInt(v, 10), true
	case uint:
		return strconv.FormatUint(uint64(v), 10), true
	case uint8:
		return strconv.FormatUint(uint64(v), 10), true
	case uint16:
		return strconv.FormatUint(uint64(v), 10), true
	case uint32:
		return strconv.FormatUint(uint64(v), 10), true
	case uint64:
		return strconv.FormatUint(v, 10), true
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 32), true
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	case []string:
		return strings.Join(v, ","), true
	case []any:
		items := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := stringifyConfigValue(item)
			if ok {
				items = append(items, s)
			}
		}
		return strings.Join(items, ","), true
	default:
		return fmt.Sprintf("%v", v), true
	}
}

func formatConfigFileYAMLError(path string, err error) error {
	line := 0
	if matches := yamlLineRe.FindStringSubmatch(err.Error()); len(matches) > 1 {
		if n, parseErr := strconv.Atoi(matches[1]); parseErr == nil {
			line = n
		}
	}
	if line > 0 {
		return fmt.Errorf("invalid CONFIG_FILE %q: malformed YAML at line %d (no secret values are echoed)", path, line)
	}
	return fmt.Errorf("invalid CONFIG_FILE %q: malformed YAML (no secret values are echoed)", path)
}
