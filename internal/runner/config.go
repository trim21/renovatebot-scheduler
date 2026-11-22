package runner

import (
	"fmt"
	"os"
	"strings"
)

// LoadExtraConfig returns the Renovate config file content from the
// environment. RENOVATE_CONFIG holds the JSON inline, RENOVATE_CONFIG_FILE
// points to a file readable by the scheduler. Setting both is an error.
//
// The content is passed to Renovate as its config file, so it can use any
// format Renovate itself accepts.
func LoadExtraConfig() ([]byte, error) {
	inline := strings.TrimSpace(os.Getenv("RENOVATE_CONFIG"))
	path := strings.TrimSpace(os.Getenv("RENOVATE_CONFIG_FILE"))

	switch {
	case inline != "" && path != "":
		return nil, fmt.Errorf("set only one of RENOVATE_CONFIG and RENOVATE_CONFIG_FILE")
	case inline != "":
		return []byte(inline), nil
	case path != "":
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read RENOVATE_CONFIG_FILE: %w", err)
		}
		return data, nil
	default:
		return nil, nil
	}
}
