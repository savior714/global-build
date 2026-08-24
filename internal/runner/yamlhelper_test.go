package runner

import (
	"strings"

	"go.yaml.in/yaml/v3"
)

func newYAMLDecoder(text string) *yaml.Decoder {
	return yaml.NewDecoder(strings.NewReader(text))
}
